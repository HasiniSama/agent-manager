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

package scorewriter

import (
	"bytes"
	"context"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wso2/agent-manager/agent-manager-observer/config"
)

const (
	thunderKID      = "thunder-test"
	thunderIssuer   = "http://thunder.amp.localhost:8080"
	serviceClientID = "amp-api-client"
	otherMonitorID  = "2c4e6a80-3b5d-4f7a-9c1e-8d2f4a6b8c0d"
	deletePath      = "/internal/v1/scores/delete"
	backfillPath    = "/internal/v1/scores/backfill"
)

// fakeDeleter records the filters it is given.
type fakeDeleter struct {
	mu      sync.Mutex
	filters []*deleteFilter
	deleted int64
	err     error
}

func (f *fakeDeleter) deleteScores(_ context.Context, filter *deleteFilter) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters = append(f.filters, filter)
	return f.deleted, f.err
}

func (f *fakeDeleter) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.filters)
}

// newInternalTestWriter is a writer with a fixed clock whose client-token keys come from jwksURL.
func newInternalTestWriter(jwksURL string, st scoreStore, del scoreDeleter, backfill bool) *Writer {
	v := newServiceTokenVerifier(config.InternalAuthConfig{
		JWKSURL:  jwksURL,
		Issuer:   []string{"Agent Management Platform Local", thunderIssuer},
		ClientID: serviceClientID,
	}, http.DefaultClient)
	clock := func() time.Time { return testNow }
	v.now, v.keys.now = clock, clock
	return &Writer{store: st, deleter: del, serviceTokens: v, backfill: backfill, retention: 30 * 24 * time.Hour, now: clock}
}

// validServiceClaims are agent-manager-service's client-credentials claims for the writer's audience, with no scope.
func validServiceClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":        thunderIssuer,
		"aud":        config.DefaultScoreWriterAudience,
		"sub":        serviceClientID,
		"client_id":  serviceClientID,
		"grant_type": "client_credentials",
		"iat":        testNow.Add(-time.Minute).Unix(),
		"exp":        testNow.Add(time.Hour).Unix(),
	}
}

func doInternal(t *testing.T, w *Writer, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	w.internalHandler().ServeHTTP(rec, req)
	return rec
}

// backfillBody is one run's scores with the identity validRunClaims carries.
func backfillBody(t *testing.T, mutate func(map[string]any)) []byte {
	t.Helper()
	m := map[string]any{
		"orgId": "org-1", "monitorId": testMonitorID, "runId": testRunID, "runStartedAt": testNow.Add(-time.Minute),
		"projectName": "proj", "agentName": "agent", "environmentName": "dev", "monitorName": "Quality",
		"individualScores": []map[string]any{{
			"evaluatorName": "Latency", "level": "trace", "traceId": traceIDFor(0),
			"traceStartTime": testNow.Add(-time.Hour), "score": 0.5,
		}},
	}
	if mutate != nil {
		mutate(m)
	}
	return mustJSON(t, m)
}

func TestDelete_ServiceClientDeletes(t *testing.T) {
	_, tk := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{thunderKID: &tk.PublicKey})
	del := &fakeDeleter{deleted: 7}
	w := newInternalTestWriter(srv.URL, &fakeStore{}, del, false)
	audit := captureAudit(t)
	token := sign(t, tk, thunderKID, validServiceClaims())
	before := testNow.Add(-time.Hour)

	rec := doInternal(t, w, deletePath, token, mustJSON(t, map[string]any{
		"orgId": "org-1", "monitorIds": []string{testMonitorID, otherMonitorID}, "evaluatorName": "Latency", "runStartedBefore": before,
	}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"traceCount":7`) {
		t.Errorf("body = %s", got)
	}
	if del.calls() != 1 {
		t.Fatalf("delete calls = %d, want 1", del.calls())
	}
	f := del.filters[0]
	if f.OrgID != "org-1" || len(f.MonitorIDs) != 2 || f.MonitorIDs[0] != testMonitorID || f.MonitorIDs[1] != otherMonitorID {
		t.Errorf("filter = %+v", f)
	}
	if f.EvaluatorName == nil || *f.EvaluatorName != "Latency" || f.RunStartedBefore == nil || !f.RunStartedBefore.Equal(before) {
		t.Errorf("evaluator filter = %v %v", f.EvaluatorName, f.RunStartedBefore)
	}

	lines := audit()
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1", len(lines))
	}
	want := map[string]any{
		"action": "score.delete", "outcome": "deleted", "status": float64(200), "clientId": serviceClientID,
		"orgId": "org-1", "monitorCount": float64(2), "evaluatorName": "Latency", "traces": float64(7),
	}
	for k, v := range want {
		if lines[0][k] != v {
			t.Errorf("audit %s = %v, want %v", k, lines[0][k], v)
		}
	}
	if strings.Contains(fmt.Sprint(lines[0]), token[len(token)-20:]) {
		t.Error("the audit line holds the token")
	}
}

func TestDelete_WholeMonitorsWithoutEvaluator(t *testing.T) {
	_, tk := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{thunderKID: &tk.PublicKey})
	del := &fakeDeleter{}
	w := newInternalTestWriter(srv.URL, &fakeStore{}, del, false)

	rec := doInternal(t, w, deletePath, sign(t, tk, thunderKID, validServiceClaims()),
		mustJSON(t, map[string]any{"orgId": "org-1", "monitorIds": []string{testMonitorID}}))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if del.calls() != 1 || del.filters[0].EvaluatorName != nil || del.filters[0].RunStartedBefore != nil {
		t.Errorf("filters = %+v, want one without an evaluator", del.filters)
	}
}

func TestInternal_RejectsTokens(t *testing.T) {
	rk, tk := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{thunderKID: &tk.PublicKey})
	with := func(mutate func(jwt.MapClaims)) jwt.MapClaims {
		c := validServiceClaims()
		mutate(c)
		return c
	}
	allScopes := "amp:monitor:score-publish amp:monitor:score-read amp:monitor:delete amp:agent:delete amp:project:delete amp:org:view"
	runForWriter := validRunClaims()
	runForWriter["aud"] = config.DefaultScoreWriterAudience
	tests := []struct {
		name     string
		token    string
		status   int
		clientID string
	}{
		{"no token", "", http.StatusUnauthorized, ""},
		{"user token with every amp scope", sign(t, tk, thunderKID, jwt.MapClaims{
			"iss": thunderIssuer, "aud": "urn:wso2:amp", "sub": "admin", "client_id": "amp-console-client",
			"ouId": "org-1", "ouHandle": "default", "scope": allScopes, "iat": testNow.Unix(), "exp": testNow.Add(time.Hour).Unix(),
		}), http.StatusUnauthorized, ""},
		{"user token for the writer's audience", sign(t, tk, thunderKID, jwt.MapClaims{
			"iss": thunderIssuer, "aud": config.DefaultScoreWriterAudience, "sub": "admin", "client_id": "amp-console-client",
			"ouId": "org-1", "scope": allScopes, "iat": testNow.Unix(), "exp": testNow.Add(time.Hour).Unix(),
		}), http.StatusForbidden, "amp-console-client"},
		{"another client's token with every amp scope", sign(t, tk, thunderKID, with(func(c jwt.MapClaims) {
			c["client_id"], c["sub"], c["scope"] = "amp-publisher-client", "amp-publisher-client", allScopes
		})), http.StatusForbidden, "amp-publisher-client"},
		{"no client_id claim", sign(t, tk, thunderKID, with(func(c jwt.MapClaims) { delete(c, "client_id") })), http.StatusForbidden, ""},
		{"agent-manager-service's token for another audience", sign(t, tk, thunderKID, with(func(c jwt.MapClaims) { c["aud"] = serviceClientID })), http.StatusUnauthorized, ""},
		{"run token", sign(t, rk, runKID, validRunClaims()), http.StatusUnauthorized, ""},
		{"run token for the writer's audience", sign(t, rk, runKID, runForWriter), http.StatusUnauthorized, ""},
		{"wrong issuer", sign(t, tk, thunderKID, with(func(c jwt.MapClaims) { c["iss"] = "https://thunder.example.com" })), http.StatusUnauthorized, ""},
		{"expired past the leeway", sign(t, tk, thunderKID, with(func(c jwt.MapClaims) { c["exp"] = testNow.Add(-31 * time.Second).Unix() })), http.StatusUnauthorized, ""},
		{"missing exp", sign(t, tk, thunderKID, with(func(c jwt.MapClaims) { delete(c, "exp") })), http.StatusUnauthorized, ""},
		{"unknown key", sign(t, tk, "thunder-other", validServiceClaims()), http.StatusUnauthorized, ""},
		{"signed by another key under the Thunder kid", sign(t, rk, thunderKID, validServiceClaims()), http.StatusUnauthorized, ""},
		{"HS256 with the public key as secret", func() string {
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validServiceClaims())
			tok.Header["kid"] = thunderKID
			s, _ := tok.SignedString(tk.N.Bytes())
			return s
		}(), http.StatusUnauthorized, ""},
	}
	routes := map[string][]byte{
		deletePath:   mustJSON(t, map[string]any{"orgId": "org-1", "monitorIds": []string{testMonitorID}}),
		backfillPath: backfillBody(t, nil),
	}
	for _, tt := range tests {
		for path, body := range routes {
			t.Run(tt.name+" on "+path, func(t *testing.T) {
				st, del := &fakeStore{}, &fakeDeleter{}
				w := newInternalTestWriter(srv.URL, st, del, true)
				audit := captureAudit(t)

				rec := doInternal(t, w, path, tt.token, body)
				if rec.Code != tt.status {
					t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
				}
				if st.calls() != 0 || del.calls() != 0 {
					t.Error("the store was called")
				}
				lines := audit()
				if len(lines) != 1 || lines[0]["outcome"] != "rejected" || lines[0]["reason"] == nil {
					t.Fatalf("audit = %v, want one rejected line with a reason", lines)
				}
				if tt.clientID != "" && lines[0]["clientId"] != tt.clientID {
					t.Errorf("audit clientId = %v, want %s", lines[0]["clientId"], tt.clientID)
				}
			})
		}
	}
}

func TestInternal_OtherTokenKindsFetchNoKeys(t *testing.T) {
	rk, tk := testKeys(t)
	srv, fetches := jwksServer(t, map[string]*rsa.PublicKey{thunderKID: &tk.PublicKey})
	w := newInternalTestWriter(srv.URL, &fakeStore{}, &fakeDeleter{}, false)
	body := mustJSON(t, map[string]any{"orgId": "org-1", "monitorIds": []string{testMonitorID}})

	for _, token := range []string{
		sign(t, rk, runKID, validRunClaims()),
		sign(t, tk, thunderKID, jwt.MapClaims{"iss": thunderIssuer, "aud": "urn:wso2:amp", "exp": testNow.Add(time.Hour).Unix()}),
	} {
		if rec := doInternal(t, w, deletePath, token, body); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	}
	if got := fetches.Load(); got != 0 {
		t.Errorf("key fetches = %d, want 0", got)
	}
}

func TestDelete_Validation(t *testing.T) {
	_, tk := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{thunderKID: &tk.PublicKey})
	token := sign(t, tk, thunderKID, validServiceClaims())
	before := testNow.Add(-time.Hour)
	body := func(mutate func(map[string]any)) []byte {
		m := map[string]any{"orgId": "org-1", "monitorIds": []string{testMonitorID}}
		mutate(m)
		return mustJSON(t, m)
	}
	ids := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("00000000-0000-4000-8000-%012d", i)
		}
		return out
	}
	tests := []struct {
		name, field string
		body        []byte
	}{
		{"evaluatorName without runStartedBefore", "runStartedBefore", body(func(m map[string]any) { m["evaluatorName"] = "Latency" })},
		{"runStartedBefore without evaluatorName", "evaluatorName", body(func(m map[string]any) { m["runStartedBefore"] = before })},
		{"empty evaluatorName", "evaluatorName", body(func(m map[string]any) { m["evaluatorName"], m["runStartedBefore"] = "", before })},
		{"runStartedBefore 6 minutes ahead", "runStartedBefore", body(func(m map[string]any) {
			m["evaluatorName"], m["runStartedBefore"] = "Latency", testNow.Add(6*time.Minute)
		})},
		{"no monitorIds", "monitorIds", body(func(m map[string]any) { delete(m, "monitorIds") })},
		{"101 monitorIds", "monitorIds", body(func(m map[string]any) { m["monitorIds"] = ids(101) })},
		{"monitor ID not a UUID", "monitorIds[0]", body(func(m map[string]any) { m["monitorIds"] = []string{"monitor-1"} })},
		{"no orgId", "orgId", body(func(m map[string]any) { delete(m, "orgId") })},
		{"a project field", "projectName", body(func(m map[string]any) { m["projectName"] = "proj" })},
		{"trailing data", "", append(body(func(map[string]any) {}), []byte(`}`)...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			del := &fakeDeleter{}
			w := newInternalTestWriter(srv.URL, &fakeStore{}, del, false)
			rec := doInternal(t, w, deletePath, token, tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), `"field":"`+tt.field+`"`) {
				t.Errorf("body = %s, want field %q", rec.Body, tt.field)
			}
			if del.calls() != 0 {
				t.Error("the store was called")
			}
		})
	}

	t.Run("100 monitorIds is allowed", func(t *testing.T) {
		w := newInternalTestWriter(srv.URL, &fakeStore{}, &fakeDeleter{}, false)
		if rec := doInternal(t, w, deletePath, token, body(func(m map[string]any) { m["monitorIds"] = ids(100) })); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
		}
	})
	t.Run("body over 64 KiB", func(t *testing.T) {
		del := &fakeDeleter{}
		w := newInternalTestWriter(srv.URL, &fakeStore{}, del, false)
		b := body(func(map[string]any) {})
		b = append(b, bytes.Repeat([]byte(" "), 64<<10)...)
		if rec := doInternal(t, w, deletePath, token, b); rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
		if del.calls() != 0 {
			t.Error("the store was called")
		}
	})
}

func TestDelete_StoreFailureIs503(t *testing.T) {
	_, tk := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{thunderKID: &tk.PublicKey})
	w := newInternalTestWriter(srv.URL, &fakeStore{}, &fakeDeleter{err: context.DeadlineExceeded}, false)
	audit := captureAudit(t)

	rec := doInternal(t, w, deletePath, sign(t, tk, thunderKID, validServiceClaims()),
		mustJSON(t, map[string]any{"orgId": "org-1", "monitorIds": []string{testMonitorID}}))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if lines := audit(); len(lines) != 1 || lines[0]["outcome"] != "failed" {
		t.Errorf("audit = %v, want one failed line", lines)
	}
}

func TestBackfill_OffIs404(t *testing.T) {
	_, tk := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{thunderKID: &tk.PublicKey})
	st := &fakeStore{}
	w := newInternalTestWriter(srv.URL, st, &fakeDeleter{}, false)

	for _, token := range []string{"", sign(t, tk, thunderKID, validServiceClaims())} {
		if rec := doInternal(t, w, backfillPath, token, backfillBody(t, nil)); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	}
	if st.calls() != 0 {
		t.Error("the store was called")
	}
}

func TestBackfill_WritesWithTheBodysIdentity(t *testing.T) {
	_, tk := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{thunderKID: &tk.PublicKey})
	st := &fakeStore{}
	w := newInternalTestWriter(srv.URL, st, &fakeDeleter{}, true)
	audit := captureAudit(t)
	started := testNow.Add(-2 * time.Hour)

	rec := doInternal(t, w, backfillPath, sign(t, tk, thunderKID, validServiceClaims()), backfillBody(t, func(m map[string]any) {
		m["orgId"], m["projectName"], m["agentName"], m["environmentName"] = "org-2", "proj-2", "agent-2", "prod"
		m["runStartedAt"] = started
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if st.calls() != 1 {
		t.Fatalf("store calls = %d, want 1", st.calls())
	}
	b := st.batches[0]
	if b.OrgID != "org-2" || b.ProjectName != "proj-2" || b.AgentName != "agent-2" || b.EnvironmentName != "prod" ||
		b.MonitorID != testMonitorID || b.RunID != testRunID || b.MonitorName != "Quality" || !b.RunStartedAt.Equal(started) {
		t.Errorf("batch identity = %+v", b)
	}
	if len(b.Traces) != 1 || b.Traces[0].Score == nil || *b.Traces[0].Score != 0.5 {
		t.Errorf("trace rows = %+v", b.Traces)
	}
	lines := audit()
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1", len(lines))
	}
	want := map[string]any{
		"action": "score.backfill", "outcome": "backfilled", "status": float64(201), "clientId": serviceClientID,
		"orgId": "org-2", "monitorId": testMonitorID, "runId": testRunID, "traces": float64(1), "spans": float64(0),
	}
	for k, v := range want {
		if lines[0][k] != v {
			t.Errorf("audit %s = %v, want %v", k, lines[0][k], v)
		}
	}
}

func TestBackfill_Validation(t *testing.T) {
	_, tk := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{thunderKID: &tk.PublicKey})
	token := sign(t, tk, thunderKID, validServiceClaims())
	item := func(mutate func(map[string]any)) []map[string]any {
		m := map[string]any{"evaluatorName": "Latency", "level": "trace", "traceId": traceIDFor(0), "traceStartTime": testNow.Add(-time.Hour), "score": 0.5}
		mutate(m)
		return []map[string]any{m}
	}
	tests := []struct {
		name, field string
		mutate      func(map[string]any)
	}{
		{"no orgId", "orgId", func(m map[string]any) { delete(m, "orgId") }},
		{"no projectName", "projectName", func(m map[string]any) { m["projectName"] = " " }},
		{"no agentName", "agentName", func(m map[string]any) { delete(m, "agentName") }},
		{"no environmentName", "environmentName", func(m map[string]any) { delete(m, "environmentName") }},
		{"no monitorName", "monitorName", func(m map[string]any) { delete(m, "monitorName") }},
		{"monitorId not a UUID", "monitorId", func(m map[string]any) { m["monitorId"] = "monitor-1" }},
		{"no runId", "runId", func(m map[string]any) { delete(m, "runId") }},
		{"no runStartedAt", "runStartedAt", func(m map[string]any) { delete(m, "runStartedAt") }},
		{"runStartedAt 6 minutes ahead", "runStartedAt", func(m map[string]any) { m["runStartedAt"] = testNow.Add(6 * time.Minute) }},
		{"a publish context", "context", func(m map[string]any) { m["context"] = map[string]any{"monitorName": "Quality"} }},
		{"no scores", "individualScores", func(m map[string]any) { delete(m, "individualScores") }},
		{"score above 1", "individualScores[0].score", func(m map[string]any) {
			m["individualScores"] = item(func(i map[string]any) { i["score"] = 1.01 })
		}},
		{"traceStartTime 31 days old", "individualScores[0].traceStartTime", func(m map[string]any) {
			m["individualScores"] = item(func(i map[string]any) { i["traceStartTime"] = testNow.Add(-31 * 24 * time.Hour) })
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{}
			w := newInternalTestWriter(srv.URL, st, &fakeDeleter{}, true)
			rec := doInternal(t, w, backfillPath, token, backfillBody(t, tt.mutate))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), `"field":"`+tt.field+`"`) {
				t.Errorf("body = %s, want field %q", rec.Body, tt.field)
			}
			if st.calls() != 0 {
				t.Error("the store was called")
			}
		})
	}
}

// Past the cap on reported errors, invalid items are still not grouped.
func TestPublish_PastTheErrorCapIs400(t *testing.T) {
	rk, _ := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	items := make([]map[string]any, maxFieldErrors+10)
	for i := range items {
		items[i] = map[string]any{"evaluatorName": "Latency", "level": "trace", "traceId": traceIDFor(i), "score": 0.5}
	}
	st := &fakeStore{}
	w := newTestWriter(srv.URL, st)

	rec := doPublish(t, w, publishPath(testMonitorID, testRunID), sign(t, rk, runKID, validRunClaims()),
		mustJSON(t, map[string]any{"context": map[string]any{"monitorName": "Quality"}, "individualScores": items}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
	}
	if got := strings.Count(rec.Body.String(), `"field":`); got != maxFieldErrors {
		t.Errorf("field errors = %d, want %d", got, maxFieldErrors)
	}
	if st.calls() != 0 {
		t.Error("the store was called")
	}
}
