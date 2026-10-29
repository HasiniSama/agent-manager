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

package specclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/wso2/agent-manager/agent-manager-service/spec"
)

// Keep this outside spec/: regeneration replaces that directory, and must not
// delete the test that verifies the custom client template's error formatting.
func TestUpdateLLMProvider_ErrorIncludesAPIMessage(t *testing.T) {
	for _, status := range []int{400, 401, 404, 409, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			const message = "Another LLM provider already uses this name and version"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodPut, r.Method)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				require.NoError(t, json.NewEncoder(w).Encode(spec.ErrorResponse{Message: message}))
			}))
			defer server.Close()
			cfg := spec.NewConfiguration()
			cfg.Servers = spec.ServerConfigurations{{URL: server.URL}}
			client := spec.NewAPIClient(cfg)

			_, response, err := client.LLMProvidersAPI.UpdateLLMProvider(context.Background(), "acme", "provider-b").
				UpdateLLMProviderRequest(spec.UpdateLLMProviderRequest{}).Execute()

			require.Error(t, err)
			require.NotNil(t, response)
			require.Equal(t, status, response.StatusCode)
			require.EqualError(t, err, fmt.Sprintf("%d %s %s", status, http.StatusText(status), message))
			var apiError *spec.GenericOpenAPIError
			require.ErrorAs(t, err, &apiError)
			require.Equal(t, spec.ErrorResponse{Message: message}, apiError.Model())
		})
	}
}
