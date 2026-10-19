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

package scorewritersvc

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/wso2/agent-manager/agent-manager-service/clients/requests"
)

type fakeAuthProvider struct {
	tokens        []string
	invalidations int
}

func (f *fakeAuthProvider) GetToken(context.Context) (string, error) {
	return f.tokens[min(f.invalidations, len(f.tokens)-1)], nil
}

func (f *fakeAuthProvider) InvalidateToken() {
	f.invalidations++
}

// writeCA writes the test server's certificate as a PEM CA file.
func writeCA(t *testing.T, server *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.crt")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(block), 0o600))
	return path
}

func newTestClient(t *testing.T, server *httptest.Server, auth *fakeAuthProvider, timeout time.Duration) ScoreWriterClient {
	t.Helper()
	c, err := NewScoreWriterClient(Config{BaseURL: server.URL, CAFile: writeCA(t, server), Timeout: timeout, AuthProvider: auth})
	require.NoError(t, err)
	return c
}

func TestDeleteScores_PostsTheDeleteOverVerifiedTLS(t *testing.T) {
	before := time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)
	var got DeleteScoresRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/internal/v1/scores/delete", r.URL.Path)
		assert.Equal(t, "Bearer amp-api-client-token", r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&got))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := newTestClient(t, server, &fakeAuthProvider{tokens: []string{"amp-api-client-token"}}, 10*time.Second)

	err := client.DeleteScores(context.Background(), DeleteScoresRequest{
		OrgID: "org-1", MonitorIDs: []string{"m-1"}, EvaluatorName: "tone", RunStartedBefore: &before,
	})

	require.NoError(t, err)
	assert.Equal(t, DeleteScoresRequest{OrgID: "org-1", MonitorIDs: []string{"m-1"}, EvaluatorName: "tone", RunStartedBefore: &before}, got)
}

func TestDeleteScores_RefusesAnUnverifiedServer(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	// No CA file: the test server's self-signed certificate is not in the system pool.
	client, err := NewScoreWriterClient(Config{
		BaseURL: server.URL, Timeout: 10 * time.Second, AuthProvider: &fakeAuthProvider{tokens: []string{"t"}},
	})
	require.NoError(t, err)

	err = client.DeleteScores(context.Background(), DeleteScoresRequest{OrgID: "org-1", MonitorIDs: []string{"m-1"}})

	require.Error(t, err)
	assert.False(t, IsRejected(err))
}

func TestDeleteScores_RetriesOnceWithAFreshTokenOn401(t *testing.T) {
	var seen []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") == "Bearer stale" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	auth := &fakeAuthProvider{tokens: []string{"stale", "fresh"}}

	err := newTestClient(t, server, auth, 10*time.Second).DeleteScores(context.Background(), DeleteScoresRequest{OrgID: "org-1"})

	require.NoError(t, err)
	assert.Equal(t, 1, auth.invalidations)
	assert.Equal(t, []string{"Bearer stale", "Bearer fresh"}, seen)
}

func TestDeleteScores_ReturnsTheWritersStatus(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := newTestClient(t, server, &fakeAuthProvider{tokens: []string{"t"}}, 10*time.Second).
		DeleteScores(context.Background(), DeleteScoresRequest{OrgID: "org-1"})

	var httpErr *requests.HttpError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusServiceUnavailable, httpErr.StatusCode)
}

func TestDeleteScores_TimesOut(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(release)

	err := newTestClient(t, server, &fakeAuthProvider{tokens: []string{"t"}}, 50*time.Millisecond).
		DeleteScores(context.Background(), DeleteScoresRequest{OrgID: "org-1"})

	require.Error(t, err)
	assert.False(t, IsRejected(err))
}

func TestNewScoreWriterClient_RejectsBadConfig(t *testing.T) {
	auth := &fakeAuthProvider{tokens: []string{"t"}}
	badCA := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(badCA, []byte("not a certificate"), 0o600))
	for name, cfg := range map[string]Config{
		"plain http":      {BaseURL: "http://amp-score-writer:9199", Timeout: time.Second, AuthProvider: auth},
		"no auth":         {BaseURL: "https://amp-score-writer:9199", Timeout: time.Second},
		"no timeout":      {BaseURL: "https://amp-score-writer:9199", AuthProvider: auth},
		"missing CA file": {BaseURL: "https://amp-score-writer:9199", Timeout: time.Second, AuthProvider: auth, CAFile: "/no/such/ca.crt"},
		"CA without PEM":  {BaseURL: "https://amp-score-writer:9199", Timeout: time.Second, AuthProvider: auth, CAFile: badCA},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewScoreWriterClient(cfg)
			assert.Error(t, err)
		})
	}
}

func TestIsRejected(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&requests.HttpError{StatusCode: http.StatusBadRequest}, true},
		{&requests.HttpError{StatusCode: http.StatusNotFound}, true},
		{&requests.HttpError{StatusCode: http.StatusRequestEntityTooLarge}, true},
		{&requests.HttpError{StatusCode: http.StatusUnauthorized}, false},
		{&requests.HttpError{StatusCode: http.StatusForbidden}, false},
		{&requests.HttpError{StatusCode: http.StatusRequestTimeout}, false},
		{&requests.HttpError{StatusCode: http.StatusTooManyRequests}, false},
		{&requests.HttpError{StatusCode: http.StatusInternalServerError}, false},
		{errors.New("connection refused"), false},
		{nil, false},
	} {
		assert.Equal(t, tc.want, IsRejected(tc.err), "%v", tc.err)
	}
}
