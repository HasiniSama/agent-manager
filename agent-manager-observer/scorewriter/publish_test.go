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
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wso2/agent-manager/agent-manager-observer/config"
)

const (
	testMonitorID = "0b6f1a52-6a43-4d0e-9a1e-3a8c3f9d2e11"
	testRunID     = "7c2d9e40-1f5b-4b8a-8f3e-5d6c7b8a9f01"
	runKID        = "eval-run-test"
	runIssuer     = "urn:wso2:amp:eval-run"
	runAudience   = "urn:wso2:amp:eval-run"
)

var (
	// testNow is the writer's clock in these tests.
	testNow = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	testKeysOnce       sync.Once
	runKey, thunderKey *rsa.PrivateKey
)

func testKeys(t *testing.T) (*rsa.PrivateKey, *rsa.PrivateKey) {
	t.Helper()
	testKeysOnce.Do(func() {
		var err error
		if runKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
		if thunderKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return runKey, thunderKey
}

// jwksServer serves the given public keys and counts fetches.
func jwksServer(t *testing.T, keys map[string]*rsa.PublicKey) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		set := map[string][]map[string]string{"keys": {}}
		for kid, k := range keys {
			set["keys"] = append(set["keys"], map[string]string{
				"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(k.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(k.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(rw).Encode(set)
	}))
	t.Cleanup(srv.Close)
	return srv, &fetches
}

// validRunClaims are claims in the run-token shape, issued a minute before testNow.
func validRunClaims() jwt.MapClaims {
	iat := testNow.Add(-time.Minute)
	return jwt.MapClaims{
		"iss":             runIssuer,
		"aud":             runAudience,
		"iat":             iat.Unix(),
		"nbf":             iat.Unix(),
		"exp":             iat.Add(35 * time.Minute).Unix(),
		"jti":             testRunID,
		"ouId":            "org-1",
		"ouHandle":        "default",
		"projectName":     "proj",
		"agentName":       "agent",
		"environmentName": "dev",
		"monitorId":       testMonitorID,
		"runId":           testRunID,
	}
}

func sign(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// fakeStore records the batches it is given.
type fakeStore struct {
	mu      sync.Mutex
	batches []*publishBatch
	err     error
}

func (f *fakeStore) publish(_ context.Context, b *publishBatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, b)
	return f.err
}

func (f *fakeStore) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

// newTestWriter is a writer with a fixed clock whose run-token keys come from jwksURL.
func newTestWriter(jwksURL string, st scoreStore) *Writer {
	v := newRunTokenVerifier(config.RunTokenConfig{JWKSURL: jwksURL, Issuer: runIssuer, Audience: runAudience}, http.DefaultClient)
	clock := func() time.Time { return testNow }
	v.now, v.keys.now = clock, clock
	return &Writer{store: st, runTokens: v, retention: 30 * 24 * time.Hour, now: clock}
}

// captureAudit routes slog to a buffer and returns the audit lines logged so far.
func captureAudit(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	var mu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&lockedWriter{w: &buf, mu: &mu}, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		var lines []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == "audit" {
				lines = append(lines, m)
			}
		}
		return lines
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func publishPath(monitorID, runID string) string {
	return "/api/v1/publisher/monitors/" + monitorID + "/runs/" + runID + "/scores"
}

func doPublish(t *testing.T, w *Writer, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	w.publishHandler().ServeHTTP(rec, req)
	return rec
}

func traceIDFor(i int) string { return fmt.Sprintf("%032x", i+1) }
func spanIDFor(i int) string  { return fmt.Sprintf("%016x", i+1) }

// scoresBody builds a body with traces x evaluators trace-level items, plus spans llm items per trace.
func scoresBody(t *testing.T, traces, spans int) []byte {
	t.Helper()
	start := testNow.Add(-time.Hour).Format(time.RFC3339Nano)
	items := []map[string]any{}
	for i := 0; i < traces; i++ {
		items = append(items, map[string]any{
			"evaluatorName": "Latency", "evaluatorIdentifier": "latency", "level": "trace",
			"traceId": traceIDFor(i), "traceStartTime": start, "score": 0.9, "explanation": "fast",
		})
		for s := 0; s < spans; s++ {
			items = append(items, map[string]any{
				"evaluatorName": "Faithfulness", "evaluatorIdentifier": "faithfulness", "level": "llm",
				"traceId": traceIDFor(i), "traceStartTime": start, "score": 0.5,
				"spanContext": map[string]any{"spanId": spanIDFor(s), "model": "gpt-4o", "vendor": "openai"},
			})
		}
	}
	return mustJSON(t, map[string]any{"context": map[string]any{"monitorName": "Quality"}, "individualScores": items})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPublish_ValidTokenPublishes(t *testing.T) {
	rk, _ := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	st := &fakeStore{}
	w := newTestWriter(srv.URL, st)
	audit := captureAudit(t)
	token := sign(t, rk, runKID, validRunClaims())

	rec := doPublish(t, w, publishPath(testMonitorID, testRunID), token, scoresBody(t, 2, 2))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"traceCount":2`) || !strings.Contains(got, `"spanCount":4`) {
		t.Errorf("body = %s", got)
	}
	if st.calls() != 1 {
		t.Fatalf("store calls = %d, want 1", st.calls())
	}
	b := st.batches[0]
	if b.OrgID != "org-1" || b.ProjectName != "proj" || b.AgentName != "agent" || b.EnvironmentName != "dev" {
		t.Errorf("identity from claims = %+v", b)
	}
	if b.MonitorID != testMonitorID || b.RunID != testRunID || b.MonitorName != "Quality" {
		t.Errorf("monitor and run = %s %s %q", b.MonitorID, b.RunID, b.MonitorName)
	}
	if !b.RunStartedAt.Equal(testNow.Add(-time.Minute)) {
		t.Errorf("run_started_at = %s, want the token's iat", b.RunStartedAt)
	}
	if len(b.Traces) != 4 || len(b.Spans) != 4 {
		t.Errorf("rows = %d traces, %d spans; want 4 and 4", len(b.Traces), len(b.Spans))
	}

	lines := audit()
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1", len(lines))
	}
	want := map[string]any{
		"action": "score.publish", "outcome": "published", "status": float64(201), "orgId": "org-1",
		"monitorId": testMonitorID, "runId": testRunID, "jti": testRunID, "traces": float64(2), "spans": float64(4),
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

func TestPublish_PathMustMatchClaims(t *testing.T) {
	rk, _ := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	other := "11111111-2222-3333-4444-555555555555"
	tests := map[string]string{
		"monitorId differs": publishPath(other, testRunID),
		"runId differs":     publishPath(testMonitorID, other),
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			st := &fakeStore{}
			w := newTestWriter(srv.URL, st)
			audit := captureAudit(t)

			rec := doPublish(t, w, path, sign(t, rk, runKID, validRunClaims()), scoresBody(t, 1, 0))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if st.calls() != 0 {
				t.Error("the store was called")
			}
			lines := audit()
			if len(lines) != 1 || lines[0]["outcome"] != "rejected" || lines[0]["runId"] != testRunID || lines[0]["pathMonitorId"] == nil {
				t.Errorf("audit = %v", lines)
			}
		})
	}
}

func TestPublish_RejectsTokens(t *testing.T) {
	rk, tk := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	with := func(mutate func(jwt.MapClaims)) jwt.MapClaims {
		c := validRunClaims()
		mutate(c)
		return c
	}
	allScopes := "amp:monitor:score-publish amp:monitor:score-read amp:monitor:create amp:observability:trace-read amp:org:view"
	tests := []struct {
		name  string
		token string
	}{
		{"no token", ""},
		{"expired past the leeway", sign(t, rk, runKID, with(func(c jwt.MapClaims) { c["exp"] = testNow.Add(-31 * time.Second).Unix() }))},
		{"not yet valid past the leeway", sign(t, rk, runKID, with(func(c jwt.MapClaims) { c["nbf"] = testNow.Add(31 * time.Second).Unix() }))},
		{"wrong audience", sign(t, rk, runKID, with(func(c jwt.MapClaims) { c["aud"] = "urn:wso2:amp" }))},
		{"wrong issuer", sign(t, rk, runKID, with(func(c jwt.MapClaims) { c["iss"] = "http://thunder.amp.localhost:8080" }))},
		{"unknown key", sign(t, rk, "eval-run-other", validRunClaims())},
		{"signed by another key under the run kid", sign(t, tk, runKID, validRunClaims())},
		{"missing monitorId", sign(t, rk, runKID, with(func(c jwt.MapClaims) { delete(c, "monitorId") }))},
		{"missing ouId", sign(t, rk, runKID, with(func(c jwt.MapClaims) { delete(c, "ouId") }))},
		{"missing nbf", sign(t, rk, runKID, with(func(c jwt.MapClaims) { delete(c, "nbf") }))},
		{"missing exp", sign(t, rk, runKID, with(func(c jwt.MapClaims) { delete(c, "exp") }))},
		{"jti is not the run", sign(t, rk, runKID, with(func(c jwt.MapClaims) { c["jti"] = "another" }))},
		{"publisher-audience token", sign(t, tk, "thunder", jwt.MapClaims{
			"iss": "http://thunder.amp.localhost:8080", "aud": "amp-publisher-default", "sub": "amp-publisher-client",
			"ouId": "org-1", "scope": "amp:monitor:score-publish", "exp": testNow.Add(time.Hour).Unix(), "iat": testNow.Unix(),
		})},
		{"user token with every amp scope", sign(t, tk, "thunder", jwt.MapClaims{
			"iss": "http://thunder.amp.localhost:8080", "aud": "urn:wso2:amp", "sub": "admin",
			"ouId": "org-1", "ouHandle": "default", "scope": allScopes, "exp": testNow.Add(time.Hour).Unix(), "iat": testNow.Unix(),
		})},
		{"HS256 with the public key as secret", func() string {
			tok := jwt.NewWithClaims(jwt.SigningMethodHS256, validRunClaims())
			tok.Header["kid"] = runKID
			s, _ := tok.SignedString(rk.N.Bytes())
			return s
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{}
			w := newTestWriter(srv.URL, st)
			audit := captureAudit(t)

			rec := doPublish(t, w, publishPath(testMonitorID, testRunID), tt.token, scoresBody(t, 1, 0))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body %s", rec.Code, rec.Body)
			}
			if st.calls() != 0 {
				t.Error("the store was called")
			}
			lines := audit()
			if len(lines) != 1 || lines[0]["outcome"] != "rejected" || lines[0]["reason"] == nil {
				t.Errorf("audit = %v, want one rejected line with a reason", lines)
			}
		})
	}
}

func TestPublish_LeewayOnExpAndNbf(t *testing.T) {
	rk, _ := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	c := validRunClaims()
	c["exp"] = testNow.Add(-29 * time.Second).Unix()
	c["nbf"] = testNow.Add(29 * time.Second).Unix()
	w := newTestWriter(srv.URL, &fakeStore{})

	if rec := doPublish(t, w, publishPath(testMonitorID, testRunID), sign(t, rk, runKID, c), scoresBody(t, 1, 0)); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d within the 30 s leeway, want 201; body %s", rec.Code, rec.Body)
	}
}

func TestPublish_RejectsIdentityFieldsInBody(t *testing.T) {
	rk, _ := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	item := map[string]any{
		"evaluatorName": "Latency", "level": "trace", "traceId": traceIDFor(0),
		"traceStartTime": testNow.Add(-time.Hour).Format(time.RFC3339), "score": 0.5,
	}
	tests := []struct {
		name, field string
		body        map[string]any
	}{
		{"orgId", "orgId", map[string]any{"orgId": "org-2", "context": map[string]any{"monitorName": "m"}, "individualScores": []any{item}}},
		{"projectName", "projectName", map[string]any{"projectName": "p2", "context": map[string]any{"monitorName": "m"}, "individualScores": []any{item}}},
		{"agentName", "agentName", map[string]any{"agentName": "a2", "context": map[string]any{"monitorName": "m"}, "individualScores": []any{item}}},
		{"context.agentName", "agentName", map[string]any{"context": map[string]any{"monitorName": "m", "agentName": "a2"}, "individualScores": []any{item}}},
		{"context.environmentName", "environmentName", map[string]any{"context": map[string]any{"monitorName": "m", "environmentName": "prod"}, "individualScores": []any{item}}},
		{"item orgId", "orgId", map[string]any{"context": map[string]any{"monitorName": "m"}, "individualScores": []any{
			map[string]any{"orgId": "org-2", "evaluatorName": "Latency", "level": "trace", "traceId": traceIDFor(0), "score": 0.5},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{}
			w := newTestWriter(srv.URL, st)
			rec := doPublish(t, w, publishPath(testMonitorID, testRunID), sign(t, rk, runKID, validRunClaims()), mustJSON(t, tt.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), `"field":"`+tt.field+`"`) {
				t.Errorf("body = %s, want field %s", rec.Body, tt.field)
			}
			if st.calls() != 0 {
				t.Error("the store was called")
			}
		})
	}
}

func TestPublish_Validation(t *testing.T) {
	rk, _ := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	start := testNow.Add(-time.Hour)
	item := func(mutate func(map[string]any)) map[string]any {
		m := map[string]any{
			"evaluatorName": "Latency", "level": "trace", "traceId": traceIDFor(0),
			"traceStartTime": start.Format(time.RFC3339Nano), "score": 0.5,
		}
		mutate(m)
		return m
	}
	body := func(items ...map[string]any) []byte {
		return mustJSON(t, map[string]any{"context": map[string]any{"monitorName": "Quality"}, "individualScores": items})
	}
	many := func(n int) []byte {
		items := make([]map[string]any, n)
		for i := range items {
			items[i] = item(func(m map[string]any) { m["traceId"] = traceIDFor(i) })
		}
		return body(items...)
	}
	span := func(id string) map[string]any {
		return item(func(m map[string]any) { m["level"] = "llm"; m["spanContext"] = map[string]any{"spanId": id} })
	}
	tests := []struct {
		name, field string
		body        []byte
	}{
		{"score below 0", "individualScores[0].score", body(item(func(m map[string]any) { m["score"] = -0.01 }))},
		{"score above 1", "individualScores[0].score", body(item(func(m map[string]any) { m["score"] = 1.01 }))},
		{"traceStartTime 31 days old", "individualScores[0].traceStartTime", body(item(func(m map[string]any) {
			m["traceStartTime"] = testNow.Add(-31 * 24 * time.Hour).Format(time.RFC3339)
		}))},
		{"traceStartTime 6 minutes ahead", "individualScores[0].traceStartTime", body(item(func(m map[string]any) {
			m["traceStartTime"] = testNow.Add(6 * time.Minute).Format(time.RFC3339)
		}))},
		{"traceStartTime missing", "individualScores[0].traceStartTime", body(item(func(m map[string]any) { delete(m, "traceStartTime") }))},
		{"501 traces", "individualScores", many(501)},
		{"trace ID not hex", "individualScores[0].traceId", body(item(func(m map[string]any) { m["traceId"] = "not-a-trace" }))},
		{"unknown level", "individualScores[0].level", body(item(func(m map[string]any) { m["level"] = "span" }))},
		{"score and skipReason", "individualScores[0]", body(item(func(m map[string]any) { m["skipReason"] = "x" }))},
		{"neither score nor skipReason", "individualScores[0]", body(item(func(m map[string]any) { delete(m, "score") }))},
		{"control characters in evaluatorName", "individualScores[0].evaluatorName", body(item(func(m map[string]any) { m["evaluatorName"] = "a\nb" }))},
		{"NUL in explanation", "individualScores[0].explanation", body(item(func(m map[string]any) { m["explanation"] = "a\x00b" }))},
		{"repeated span", "individualScores[1].spanContext.spanId", body(span(spanIDFor(0)), span(spanIDFor(0)))},
		{"bad span ID", "individualScores[0].spanContext.spanId", body(span("xyz"))},
		{"level differs for one evaluator and trace", "individualScores[1].level", body(item(func(map[string]any) {}), span(spanIDFor(0)))},
		{"no monitorName", "context.monitorName", mustJSON(t, map[string]any{"individualScores": []any{item(func(map[string]any) {})}})},
		{"no scores", "individualScores", body()},
		{"score is a string", "individualScores.", body(item(func(m map[string]any) { m["score"] = "0.5" }))},
		{"trailing data", "", append(body(item(func(map[string]any) {})), []byte(`}`)...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{}
			w := newTestWriter(srv.URL, st)
			rec := doPublish(t, w, publishPath(testMonitorID, testRunID), sign(t, rk, runKID, validRunClaims()), tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body)
			}
			// A field ending in "." is a prefix: the decoder's type-error paths vary by Go version.
			want := `"field":"` + tt.field
			if !strings.HasSuffix(tt.field, ".") {
				want += `"`
			}
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("body = %s, want field %q", rec.Body, tt.field)
			}
			if st.calls() != 0 {
				t.Error("the store was called")
			}
		})
	}

	t.Run("500 traces is allowed", func(t *testing.T) {
		w := newTestWriter(srv.URL, &fakeStore{})
		if rec := doPublish(t, w, publishPath(testMonitorID, testRunID), sign(t, rk, runKID, validRunClaims()), many(500)); rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
		}
	})
}

func TestPublish_BodyCap(t *testing.T) {
	rk, _ := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	token := sign(t, rk, runKID, validRunClaims())
	padded := func(size int) []byte {
		b := scoresBody(t, 1, 0)
		return append(b, bytes.Repeat([]byte(" "), size-len(b))...)
	}

	w := newTestWriter(srv.URL, &fakeStore{})
	if rec := doPublish(t, w, publishPath(testMonitorID, testRunID), token, padded(8<<20)); rec.Code != http.StatusCreated {
		t.Errorf("8 MiB: status = %d, want 201", rec.Code)
	}
	st := &fakeStore{}
	w = newTestWriter(srv.URL, st)
	if rec := doPublish(t, w, publishPath(testMonitorID, testRunID), token, padded(8<<20+1)); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("8 MiB + 1: status = %d, want 413", rec.Code)
	}
	if st.calls() != 0 {
		t.Error("the store was called")
	}
}

func TestPublish_StoreFailureIs503(t *testing.T) {
	rk, _ := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	w := newTestWriter(srv.URL, &fakeStore{err: context.DeadlineExceeded})
	audit := captureAudit(t)

	rec := doPublish(t, w, publishPath(testMonitorID, testRunID), sign(t, rk, runKID, validRunClaims()), scoresBody(t, 1, 0))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if lines := audit(); len(lines) != 1 || lines[0]["outcome"] != "failed" {
		t.Errorf("audit = %v, want one failed line", lines)
	}
}

func TestJWKSRefreshIsRateLimited(t *testing.T) {
	rk, _ := testKeys(t)
	srv, fetches := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	w := newTestWriter(srv.URL, &fakeStore{})
	now := testNow
	clock := func() time.Time { return now }
	w.runTokens.keys.now = clock

	for i := 0; i < 50; i++ {
		rec := doPublish(t, w, publishPath(testMonitorID, testRunID), sign(t, rk, fmt.Sprintf("unknown-%d", i), validRunClaims()), scoresBody(t, 1, 0))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unknown kid %d: status = %d, want 401", i, rec.Code)
		}
	}
	if got := fetches.Load(); got != 1 {
		t.Fatalf("fetches after 50 unknown kids = %d, want 1", got)
	}
	if rec := doPublish(t, w, publishPath(testMonitorID, testRunID), sign(t, rk, runKID, validRunClaims()), scoresBody(t, 1, 0)); rec.Code != http.StatusCreated {
		t.Fatalf("known kid: status = %d, want 201", rec.Code)
	}
	now = now.Add(61 * time.Second)
	doPublish(t, w, publishPath(testMonitorID, testRunID), sign(t, rk, "unknown-late", validRunClaims()), scoresBody(t, 1, 0))
	if got := fetches.Load(); got != 2 {
		t.Errorf("fetches a minute later = %d, want 2", got)
	}
}

func TestBuildRows_TraceRowsAndMeans(t *testing.T) {
	start := testNow.Add(-time.Hour)
	f := func(v float64) *float64 { return &v }
	s := func(v string) *string { return &v }
	req := &publishRequest{
		Context: publishContext{MonitorName: "Quality"},
		IndividualScores: []scoreItem{
			{EvaluatorName: "Latency", EvaluatorIdentifier: "latency", Level: "trace", TraceID: traceIDFor(0), TraceStartTime: &start, Score: f(0.9), Explanation: s("fast")},
			{EvaluatorName: "Faithfulness", Level: "llm", TraceID: traceIDFor(0), TraceStartTime: &start, Score: f(0.2),
				SpanContext: &spanContext{SpanID: spanIDFor(0), Model: s("gpt-4o"), Vendor: s("openai")}},
			{EvaluatorName: "Faithfulness", Level: "llm", TraceID: traceIDFor(0), TraceStartTime: &start, Score: f(0.6),
				SpanContext: &spanContext{SpanID: spanIDFor(1), Model: s("gpt-4o-mini")}},
			{EvaluatorName: "Faithfulness", Level: "llm", TraceID: traceIDFor(0), TraceStartTime: &start, SkipReason: s("no output"),
				SpanContext: &spanContext{SpanID: spanIDFor(2)}},
			{EvaluatorName: "Helpfulness", Level: "agent", TraceID: traceIDFor(0), TraceStartTime: &start, SkipReason: s("evaluator raised")},
		},
	}
	traces, spans, errs := buildRows(req, testNow, 30*24*time.Hour)
	if errs != nil {
		t.Fatalf("errors: %v", errs)
	}
	if len(traces) != 3 || len(spans) != 3 {
		t.Fatalf("rows = %d traces, %d spans; want 3 and 3", len(traces), len(spans))
	}

	latency, faith, helpful := traces[0], traces[1], traces[2]
	if *latency.Score != 0.9 || latency.ScoreSum != 0.9 || latency.ScoredCount != 1 || latency.SkippedCount != 0 || *latency.Explanation != "fast" || latency.EvaluatorIdentifier != "latency" {
		t.Errorf("trace-level row = %+v", latency)
	}
	if faith.Score == nil || *faith.Score < 0.3999 || *faith.Score > 0.4001 || faith.ScoreSum < 0.7999 || faith.ScoreSum > 0.8001 ||
		faith.ScoredCount != 2 || faith.SkippedCount != 1 || faith.SkipReason != nil || faith.EvaluatorIdentifier != "Faithfulness" {
		t.Errorf("llm row = %+v, want the mean of the scored spans and the identifier defaulting to the name", faith)
	}
	if helpful.Score != nil || helpful.ScoredCount != 0 || helpful.SkippedCount != 1 || helpful.SkipReason == nil || *helpful.SkipReason != "evaluator raised" {
		t.Errorf("all-skipped agent row = %+v", helpful)
	}
	if spans[0].Label != "openai/gpt-4o" || spans[1].Label != "gpt-4o-mini" || spans[2].Label != "" || spans[2].SkipReason == nil {
		t.Errorf("span rows = %+v", spans)
	}
}

func TestPublish_KeysUnreachableIs503UntilTheyLoad(t *testing.T) {
	rk, _ := testKeys(t)
	var up atomic.Bool
	srv, fetches := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	gate := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(rw, "down", http.StatusBadGateway)
			return
		}
		resp, err := http.Get(srv.URL)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(rw, resp.Body)
	}))
	t.Cleanup(gate.Close)
	st := &fakeStore{}
	w := newTestWriter(gate.URL, st)
	now := testNow
	w.runTokens.keys.now = func() time.Time { return now }
	audit := captureAudit(t)
	token := sign(t, rk, runKID, validRunClaims())

	for i := 0; i < 2; i++ {
		if rec := doPublish(t, w, publishPath(testMonitorID, testRunID), token, scoresBody(t, 1, 0)); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("keys down, attempt %d: status = %d, want 503 so the job retries", i, rec.Code)
		}
	}
	if lines := audit(); len(lines) != 2 || lines[0]["outcome"] != "failed" || lines[1]["outcome"] != "failed" {
		t.Errorf("audit = %v, want two failed lines", lines)
	}
	up.Store(true)
	now = now.Add(61 * time.Second)
	if rec := doPublish(t, w, publishPath(testMonitorID, testRunID), token, scoresBody(t, 1, 0)); rec.Code != http.StatusCreated {
		t.Fatalf("keys back: status = %d, want 201", rec.Code)
	}
	if st.calls() != 1 || fetches.Load() != 1 {
		t.Errorf("store calls = %d, key fetches = %d; want 1 and 1", st.calls(), fetches.Load())
	}
}

func TestPublish_OtherTokenKindsFetchNoKeys(t *testing.T) {
	_, tk := testKeys(t)
	srv, fetches := jwksServer(t, nil)
	w := newTestWriter(srv.URL, &fakeStore{})
	for _, claims := range []jwt.MapClaims{
		{"iss": "http://thunder.amp.localhost:8080", "aud": "amp-publisher-default", "exp": testNow.Add(time.Hour).Unix()},
		{"iss": "http://thunder.amp.localhost:8080", "aud": "urn:wso2:amp", "scope": "amp:monitor:score-publish", "exp": testNow.Add(time.Hour).Unix()},
	} {
		if rec := doPublish(t, w, publishPath(testMonitorID, testRunID), sign(t, tk, "thunder", claims), scoresBody(t, 1, 0)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	}
	if got := fetches.Load(); got != 0 {
		t.Errorf("key fetches = %d, want 0", got)
	}
}
