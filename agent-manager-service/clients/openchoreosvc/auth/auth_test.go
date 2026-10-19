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

package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenServer answers every token request and records the form of the last one.
func tokenServer(t *testing.T, form *url.Values) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.ParseForm())
		*form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","token_type":"Bearer","expires_in":3600}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestGetToken_RequestsTheResource(t *testing.T) {
	var form url.Values
	server := tokenServer(t, &form)
	provider := NewAuthProvider(Config{TokenURL: server.URL, ClientID: "amp-api-client", ClientSecret: "secret", Resource: "urn:wso2:amp:score-writer"})

	token, err := provider.GetToken(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "token", token)
	assert.Equal(t, "client_credentials", form.Get("grant_type"))
	assert.Equal(t, "urn:wso2:amp:score-writer", form.Get("resource"))
	assert.False(t, form.Has("scope"), "a resource-only request names no scope")
}

func TestGetToken_SendsNoResourceUnlessSet(t *testing.T) {
	var form url.Values
	server := tokenServer(t, &form)
	provider := NewAuthProvider(Config{TokenURL: server.URL, ClientID: "amp-api-client", ClientSecret: "secret", Scope: "amp:org:view"})

	_, err := provider.GetToken(context.Background())

	require.NoError(t, err)
	assert.Equal(t, "amp:org:view", form.Get("scope"))
	assert.False(t, form.Has("resource"))
}
