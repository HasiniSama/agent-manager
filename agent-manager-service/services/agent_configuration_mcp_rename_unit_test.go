// Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
//
// WSO2 LLC. licenses this file to you under the Apache License,
// Version 2.0 (the "License"); you may not use this file except
// in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package services

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/wso2/agent-manager/agent-manager-service/clients/clientmocks"
	"github.com/wso2/agent-manager/agent-manager-service/clients/openchoreosvc/client"
	"github.com/wso2/agent-manager/agent-manager-service/clients/secretmanagersvc"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/repositories/repomocks"
)

// mcpRenameRecorder captures every OpenChoreo write the rename makes, so a test can assert
// both what was written and that nothing was written at all.
type mcpRenameRecorder struct {
	removedFromComponent []string
	bindingRemoved       map[string][]string
	bindingVars          map[string][]client.EnvVar
	componentVars        []client.EnvVar
	writes               int
}

func (r *mcpRenameRecorder) ocClient() *clientmocks.OpenChoreoClientMock {
	r.bindingRemoved = map[string][]string{}
	r.bindingVars = map[string][]client.EnvVar{}
	return &clientmocks.OpenChoreoClientMock{
		RemoveComponentEnvironmentVariablesFunc: func(_ context.Context, _, _, _ string, keys []string) error {
			r.writes++
			r.removedFromComponent = keys
			return nil
		},
		ReplaceReleaseBindingEnvVarsFunc: func(_ context.Context, _, _, _, envName string, keysToRemove []string, envVars []client.EnvVar) error {
			r.writes++
			r.bindingRemoved[envName] = keysToRemove
			r.bindingVars[envName] = envVars
			return nil
		},
		UpdateComponentEnvVarsFunc: func(_ context.Context, _, _, _ string, envVars []client.EnvVar) error {
			r.writes++
			r.componentVars = envVars
			return nil
		},
	}
}

func renamedTemplates() []EnvConfigTemplate {
	return []EnvConfigTemplate{
		{Key: "url", Name: "GITHUB_URL"},
		{Key: "apikey", Name: "GITHUB_API_KEY", IsSecret: true},
	}
}

func renameVarRows(configUUID, envUUID uuid.UUID, secretRef string) []models.AgentEnvConfigVariable {
	return []models.AgentEnvConfigVariable{
		{ConfigUUID: configUUID, EnvironmentUUID: envUUID, VariableKey: "url", VariableName: "GITHUB_MCP_URL"},
		{ConfigUUID: configUUID, EnvironmentUUID: envUUID, VariableKey: "apikey", VariableName: "GITHUB_MCP_API_KEY", SecretReference: secretRef},
	}
}

func envVarByKey(t *testing.T, vars []client.EnvVar, key string) client.EnvVar {
	t.Helper()
	for _, v := range vars {
		if v.Key == key {
			return v
		}
	}
	t.Fatalf("env var %q not found in %+v", key, vars)
	return client.EnvVar{}
}

// Regression for wso2/agent-manager#2164: the agent-config repository loads mappings
// without the proxy's endpoint → environment bindings, so the rename resolved no gateway
// artifact, logged a warning and left every ReleaseBinding on the old names. The rename
// must resolve the gateway from the fully loaded proxy and rewrite each environment with
// its own URL and its own SecretReference under the "api-key" property.
func TestApplyRenamedMCPEnvVars_RewritesEachEnvWithItsOwnValues(t *testing.T) {
	f := newMCPURLTestFixture()
	stageEnv := uuid.New()
	configUUID := uuid.New()

	// The mapping as the agent-config repository used to hand it over: the proxy is
	// there, its Endpoints are not.
	bareMapping := f.mapping
	fullProxy := f.mapping.MCPProxy
	bareMapping.MCPProxy = &models.MCPProxy{UUID: fullProxy.UUID, Configuration: fullProxy.Configuration}

	rec := &mcpRenameRecorder{}
	svc := f.newService()
	svc.ocClient = rec.ocClient()
	svc.envMCPMappingRepo = &repomocks.EnvAgentMCPMappingRepositoryMock{
		ListByConfigFunc: func(context.Context, uuid.UUID) ([]models.EnvAgentMCPMapping, error) {
			return []models.EnvAgentMCPMapping{bareMapping}, nil
		},
	}
	svc.mcpProxyRepo = &repomocks.MCPProxyRepositoryMock{
		GetByUUIDFunc: func(_ context.Context, proxyUUID, _ string) (*models.MCPProxy, error) {
			require.Equal(t, fullProxy.UUID.String(), proxyUUID)
			return fullProxy, nil
		},
	}

	config := &models.AgentConfiguration{UUID: configUUID, OUID: f.ouID, ProjectName: "default", AgentID: "it-support", Name: "github"}
	vars := append(renameVarRows(configUUID, f.envUUID, "github-default-proxy-secrets"), renameVarRows(configUUID, stageEnv, "")...)
	uuidToEnvName := map[string]string{f.envUUID.String(): "default", stageEnv.String(): "stage"}
	oldKeys := []string{"GITHUB_MCP_URL", "GITHUB_MCP_API_KEY"}

	err := svc.applyRenamedMCPEnvVars(context.Background(), config, f.ouID, "default", "it-support",
		uuidToEnvName, "default", vars, renamedTemplates(), oldKeys)
	require.NoError(t, err)

	require.ElementsMatch(t, oldKeys, rec.removedFromComponent)
	require.ElementsMatch(t, oldKeys, rec.bindingRemoved["default"])
	require.ElementsMatch(t, oldKeys, rec.bindingRemoved["stage"])

	defaultURL := envVarByKey(t, rec.bindingVars["default"], "GITHUB_URL")
	require.Equal(t, "https://gw.example.com/proxy-ctx/mcp", defaultURL.Value)
	defaultKey := envVarByKey(t, rec.bindingVars["default"], "GITHUB_API_KEY")
	require.NotNil(t, defaultKey.ValueFrom)
	require.Equal(t, client.SecretKeyRef{Name: "github-default-proxy-secrets", Key: secretmanagersvc.SecretKeyAPIKey},
		*defaultKey.ValueFrom.SecretKeyRef)

	// stage has variable rows but no mapping: it gets the new names, empty — never the
	// default environment's URL or secret.
	stageURL := envVarByKey(t, rec.bindingVars["stage"], "GITHUB_URL")
	require.Empty(t, stageURL.Value)
	stageKey := envVarByKey(t, rec.bindingVars["stage"], "GITHUB_API_KEY")
	require.Nil(t, stageKey.ValueFrom)
	require.Empty(t, stageKey.Value)

	require.Equal(t, rec.bindingVars["default"], rec.componentVars, "the Component base must carry the first environment's renamed vars")
}

// A gateway that cannot be resolved must fail the rename before anything is written, so the
// caller's transaction rolls the DB rename back and the Component and ReleaseBindings are
// never left half on the old names and half on the new ones.
func TestApplyRenamedMCPEnvVars_ResolutionFailureWritesNothing(t *testing.T) {
	f := newMCPURLTestFixture()
	configUUID := uuid.New()

	rec := &mcpRenameRecorder{}
	svc := f.newService()
	svc.ocClient = rec.ocClient()
	svc.gatewayRepo = &repomocks.GatewayRepositoryMock{
		EnvironmentMappingExistsFunc: func(string, string) (bool, error) {
			return false, errors.New("db unavailable")
		},
	}
	svc.envMCPMappingRepo = &repomocks.EnvAgentMCPMappingRepositoryMock{
		ListByConfigFunc: func(context.Context, uuid.UUID) ([]models.EnvAgentMCPMapping, error) {
			return []models.EnvAgentMCPMapping{f.mapping}, nil
		},
	}

	config := &models.AgentConfiguration{UUID: configUUID, OUID: f.ouID, ProjectName: "default", AgentID: "it-support", Name: "github"}
	err := svc.applyRenamedMCPEnvVars(context.Background(), config, f.ouID, "default", "it-support",
		map[string]string{f.envUUID.String(): "default"}, "default",
		renameVarRows(configUUID, f.envUUID, "github-default-proxy-secrets"), renamedTemplates(),
		[]string{"GITHUB_MCP_URL", "GITHUB_MCP_API_KEY"})

	require.Error(t, err)
	require.Zero(t, rec.writes, "no OpenChoreo resource may be touched once resolution has failed")
}
