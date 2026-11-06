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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/wso2/agent-manager/agent-manager-service/config"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/repositories/repomocks"
)

func TestMonitorExecutor_resolveOTelEndpoint(t *testing.T) {
	env := uuid.New()
	ingress := newGateway(t, models.GatewayRoleIngress, true)
	egress := newGateway(t, models.GatewayRoleEgress, true)
	monitor := &models.Monitor{OUID: "org", EnvironmentID: env.String()}

	t.Run("uses the ingress gateway's runtime URL", func(t *testing.T) {
		e := &monitorExecutor{gatewayRepo: gatewayFixtureRepo(t, env.String(), []*models.Gateway{egress, ingress})}

		url, err := e.resolveOTelEndpoint(monitor)

		require.NoError(t, err)
		assert.Equal(t, ingress.RuntimeURL+"/otel", url)
	})

	t.Run("uses the public vhost when evaluation jobs run outside the cluster", func(t *testing.T) {
		cfg := config.GetConfig()
		orig := cfg.EvalJobsUsePublicGatewayURL
		cfg.EvalJobsUsePublicGatewayURL = true
		t.Cleanup(func() { cfg.EvalJobsUsePublicGatewayURL = orig })
		e := &monitorExecutor{gatewayRepo: gatewayFixtureRepo(t, env.String(), []*models.Gateway{ingress})}

		url, err := e.resolveOTelEndpoint(monitor)

		require.NoError(t, err)
		assert.Equal(t, "https://"+ingress.Vhost+"/otel", url)
	})

	t.Run("fails when the environment has no ingress gateway", func(t *testing.T) {
		e := &monitorExecutor{gatewayRepo: gatewayFixtureRepo(t, env.String(), []*models.Gateway{egress})}

		_, err := e.resolveOTelEndpoint(monitor)

		assert.ErrorIs(t, err, errNoGatewayForEnvironment)
	})
}

func TestMonitorExecutor_buildWorkflowRunRequest_OTelEndpoint(t *testing.T) {
	env := uuid.New()
	ingress := newGateway(t, models.GatewayRoleBoth, true)
	monitor := &models.Monitor{ID: uuid.New(), OUID: "org", Name: "my-monitor", EnvironmentID: env.String()}
	evaluators := []models.MonitorEvaluator{
		{Identifier: "latency_performance", DisplayName: "Latency", Config: map[string]interface{}{"level": "trace", "max_latency_ms": float64(3000)}},
	}
	credRepo := &repomocks.OrgPublisherCredentialRepositoryMock{
		GetByOrgNameFunc: func(string) (*models.OrgPublisherCredential, error) { return nil, gorm.ErrRecordNotFound },
	}
	newExecutor := func(gateways []*models.Gateway) *monitorExecutor {
		return &monitorExecutor{
			logger:       discardLogger(),
			credRepo:     credRepo,
			provisioner:  NewStaticPublisherCredentialProvisioner(),
			custEvalRepo: &repomocks.CustomEvaluatorRepositoryMock{},
			gatewayRepo:  gatewayFixtureRepo(t, env.String(), gateways),
		}
	}
	build := func(e *monitorExecutor) (map[string]interface{}, error) {
		req, err := e.buildWorkflowRunRequest(context.Background(), monitor, uuid.New(),
			time.Now().Add(-time.Hour), time.Now(), evaluators, "", "", "", "ns")
		if err != nil {
			return nil, err
		}
		return req.Parameters["publishing"].(map[string]interface{}), nil
	}

	t.Run("passes the agent's /otel URL", func(t *testing.T) {
		publishing, err := build(newExecutor([]*models.Gateway{ingress}))

		require.NoError(t, err)
		assert.Equal(t, ingress.RuntimeURL+"/otel", publishing["otelEndpoint"])
	})

	t.Run("still builds the run when no gateway is found", func(t *testing.T) {
		publishing, err := build(newExecutor([]*models.Gateway{}))

		require.NoError(t, err)
		assert.Equal(t, "", publishing["otelEndpoint"])
	})
}
