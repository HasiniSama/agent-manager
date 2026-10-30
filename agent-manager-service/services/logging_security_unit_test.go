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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/repositories/repomocks"
	"github.com/wso2/agent-manager/agent-manager-service/utils"
)

// captureSecurityLogs restores the global logger; callers must not run in parallel.
func captureSecurityLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func TestLLMProxyCreate_InvalidConfigurationLogsFixedKeys(t *testing.T) {
	for _, missing := range []string{"name", "version", "provider"} {
		t.Run(missing, func(t *testing.T) {
			buf := captureSecurityLogs(t)
			// These would overwrite trusted metadata if used as attribute keys.
			cfg := models.LLMProxyConfig{Name: "level", Version: "msg", Provider: "provider"}
			switch missing {
			case "name":
				cfg.Name = ""
			case "version":
				cfg.Version = ""
			case "provider":
				cfg.Provider = ""
			}
			svc := NewLLMProxyService(&repomocks.LLMProxyRepositoryMock{}, &repomocks.LLMProviderRepositoryMock{}, nil)
			result, err := svc.Create(context.Background(), "org", "actor", &models.LLMProxy{Configuration: cfg})
			require.ErrorIs(t, err, utils.ErrInvalidInput)
			require.Nil(t, result)
			var record map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
			require.Equal(t, "ERROR", record["level"])
			require.Equal(t, "handle, name, version or provider is empty", record["msg"])
			require.Equal(t, cfg.Name == "", record["nameMissing"])
			require.Equal(t, cfg.Version == "", record["versionMissing"])
			require.Equal(t, cfg.Provider == "", record["providerMissing"])
			require.Len(t, record, 6)
		})
	}
}

func TestVerifyToken_MalformedInputIsNotLogged(t *testing.T) {
	for _, tc := range []struct{ name, token, message string }{
		{"empty", "", "token verification failed: empty token"},
		{"short", "sensitive-input", "token verification failed: invalid token format"},
		{"injected", "secret\r\nforged", "token verification failed: invalid token format"},
		{"invalid UUID", "sensitive-input-with-five-parts-secret", "token verification failed: invalid UUID prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureSecurityLogs(t)
			svc := NewPlatformGatewayService(&repomocks.GatewayRepositoryMock{}, nil)
			result, err := svc.VerifyToken(context.Background(), tc.token)
			require.Error(t, err)
			require.Nil(t, result)
			var record map[string]any
			require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
			require.Equal(t, "WARN", record["level"])
			require.Equal(t, tc.message, record["msg"])
			require.Len(t, record, 3, "failure logs must contain only time, level and a fixed message")
		})
	}
}
