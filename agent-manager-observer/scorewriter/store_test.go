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
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testDatabases gives a fresh score database with the migrations applied, as the admin and as the writer.
// SCORE_STORE_TEST_DSN is an admin DSN for a scratch Postgres; the tests skip without it.
func testDatabases(t *testing.T) (admin, writer *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("SCORE_STORE_TEST_DSN")
	if dsn == "" {
		t.Skip("SCORE_STORE_TEST_DSN is not set")
	}
	ctx := context.Background()
	root, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = root.Close(ctx) })
	name := fmt.Sprintf("amp_scores_test_%d", time.Now().UnixNano())
	if _, err := root.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() { _, _ = root.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)") })

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	admin, err = pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	files, err := filepath.Glob("../scorestore/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		sql, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	if _, err := admin.Exec(ctx, "ALTER ROLE amp_scores_writer LOGIN PASSWORD 'writer-test'"); err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("amp_scores_writer", "writer-test")
	writer, err = pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(writer.Close)
	return admin, writer
}

// dbWriter is a test writer that stores through the writer login.
func dbWriter(t *testing.T, writerPool *pgxpool.Pool) (*Writer, *rsa.PrivateKey) {
	t.Helper()
	rk, _ := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{runKID: &rk.PublicKey})
	return newTestWriter(srv.URL, &store{pool: writerPool}), rk
}

func publishOK(t *testing.T, w *Writer, token string, body []byte) {
	t.Helper()
	rec := doPublish(t, w, publishPath(testMonitorID, testRunID), token, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("publish status = %d, body %s", rec.Code, rec.Body)
	}
}

type traceScoreRow struct {
	OrgID, Project, Agent, Env, MonitorName, RunID, Identifier, Level string
	RunStartedAt, TraceStart                                          time.Time
	Score                                                             *float64
	ScoreSum                                                          float64
	Scored, Skipped                                                   int32
	SkipReason, Explanation                                           *string
}

func readTraceRow(t *testing.T, admin *pgxpool.Pool, evaluator, traceID string) traceScoreRow {
	t.Helper()
	var r traceScoreRow
	err := admin.QueryRow(context.Background(), `
		SELECT org_id, project_name, agent_name, environment_name, monitor_name, run_id::text,
		       evaluator_identifier, level, run_started_at, trace_start_time, score::float8,
		       score_sum, scored_count, skipped_count, skip_reason, explanation
		FROM trace_scores WHERE monitor_id = $1 AND evaluator_name = $2 AND trace_id = $3`,
		testMonitorID, evaluator, traceID,
	).Scan(&r.OrgID, &r.Project, &r.Agent, &r.Env, &r.MonitorName, &r.RunID, &r.Identifier, &r.Level,
		&r.RunStartedAt, &r.TraceStart, &r.Score, &r.ScoreSum, &r.Scored, &r.Skipped, &r.SkipReason, &r.Explanation)
	if err != nil {
		t.Fatalf("read %s/%s: %v", evaluator, traceID, err)
	}
	return r
}

func countRows(t *testing.T, admin *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	if err := admin.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStore_RowShapes(t *testing.T) {
	admin, writerPool := testDatabases(t)
	w, rk := dbWriter(t, writerPool)
	start := testNow.Add(-time.Hour).Truncate(time.Millisecond)
	body := mustJSON(t, map[string]any{
		"context": map[string]any{"monitorName": "Quality"},
		"individualScores": []map[string]any{
			{"evaluatorName": "Latency", "evaluatorIdentifier": "latency", "level": "trace", "traceId": traceIDFor(0),
				"traceStartTime": start, "score": 0.9, "explanation": "fast"},
			{"evaluatorName": "Faithfulness", "evaluatorIdentifier": "faithfulness", "level": "llm", "traceId": traceIDFor(0),
				"traceStartTime": start, "score": 0.25, "explanation": "partly",
				"spanContext": map[string]any{"spanId": spanIDFor(0), "model": "gpt-4o", "vendor": "openai"}},
			{"evaluatorName": "Faithfulness", "evaluatorIdentifier": "faithfulness", "level": "llm", "traceId": traceIDFor(0),
				"traceStartTime": start, "score": 0.75,
				"spanContext": map[string]any{"spanId": spanIDFor(1), "model": "gpt-4o-mini"}},
			{"evaluatorName": "Faithfulness", "evaluatorIdentifier": "faithfulness", "level": "llm", "traceId": traceIDFor(0),
				"traceStartTime": start, "skipReason": "no output",
				"spanContext": map[string]any{"spanId": spanIDFor(2)}},
		},
	})
	publishOK(t, w, sign(t, rk, runKID, validRunClaims()), body)

	latency := readTraceRow(t, admin, "Latency", traceIDFor(0))
	iat := testNow.Add(-time.Minute)
	if latency.OrgID != "org-1" || latency.Project != "proj" || latency.Agent != "agent" || latency.Env != "dev" ||
		latency.MonitorName != "Quality" || latency.RunID != testRunID || latency.Identifier != "latency" || latency.Level != "trace" {
		t.Errorf("trace-level identity = %+v", latency)
	}
	if !latency.RunStartedAt.Equal(iat) || !latency.TraceStart.Equal(start) {
		t.Errorf("run_started_at = %s (want iat %s), trace_start_time = %s", latency.RunStartedAt, iat, latency.TraceStart)
	}
	if latency.Score == nil || *latency.Score != 0.9 || latency.ScoreSum != 0.9 || latency.Scored != 1 || latency.Skipped != 0 ||
		latency.Explanation == nil || *latency.Explanation != "fast" || latency.SkipReason != nil {
		t.Errorf("trace-level score columns = %+v", latency)
	}

	faith := readTraceRow(t, admin, "Faithfulness", traceIDFor(0))
	if faith.Score == nil || *faith.Score != 0.5 || faith.ScoreSum != 1.0 || faith.Scored != 2 || faith.Skipped != 1 {
		t.Errorf("llm mean columns = score %v, sum %v, scored %d, skipped %d; want 0.5, 1, 2, 1",
			faith.Score, faith.ScoreSum, faith.Scored, faith.Skipped)
	}
	// The mean across evaluators is sum(score_sum) / sum(scored_count), as today's AVG over raw rows.
	var mean float64
	if err := admin.QueryRow(context.Background(),
		`SELECT sum(score_sum) / sum(scored_count) FROM trace_scores WHERE trace_id = $1`, traceIDFor(0)).Scan(&mean); err != nil {
		t.Fatal(err)
	}
	if want := (0.9 + 0.25 + 0.75) / 3; mean < want-1e-9 || mean > want+1e-9 {
		t.Errorf("mean across evaluators = %v, want %v", mean, want)
	}

	rows, err := admin.Query(context.Background(),
		`SELECT span_id, label, score::float8, skip_reason, explanation FROM span_scores
		 WHERE evaluator_name = 'Faithfulness' AND trace_id = $1 ORDER BY span_id`, traceIDFor(0))
	if err != nil {
		t.Fatal(err)
	}
	type span struct {
		ID, Label         string
		Score             *float64
		Skip, Explanation *string
	}
	spans, err := pgx.CollectRows(rows, pgx.RowToStructByPos[span])
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 3 {
		t.Fatalf("span rows = %d, want 3", len(spans))
	}
	if spans[0].Label != "openai/gpt-4o" || *spans[0].Score != 0.25 || *spans[0].Explanation != "partly" ||
		spans[1].Label != "gpt-4o-mini" || *spans[1].Score != 0.75 ||
		spans[2].Label != "" || spans[2].Score != nil || *spans[2].Skip != "no output" {
		t.Errorf("span rows = %+v", spans)
	}
}

// A re-run replaces this evaluator's row and spans and keeps other evaluators' rows.
// Today's DeleteStaleScores would remove those; the writer leaves them to the delete path.
func TestStore_RerunReplacesOnlyItsEvaluator(t *testing.T) {
	admin, writerPool := testDatabases(t)
	w, rk := dbWriter(t, writerPool)
	start := testNow.Add(-time.Hour)
	item := func(evaluator, level string, score float64, spanID string) map[string]any {
		m := map[string]any{"evaluatorName": evaluator, "level": level, "traceId": traceIDFor(0), "traceStartTime": start, "score": score}
		if spanID != "" {
			m["spanContext"] = map[string]any{"spanId": spanID, "model": "gpt-4o"}
		}
		return m
	}
	first := mustJSON(t, map[string]any{"context": map[string]any{"monitorName": "Quality"}, "individualScores": []map[string]any{
		item("Latency", "trace", 0.9, ""),
		item("Faithfulness", "llm", 0.2, spanIDFor(0)), item("Faithfulness", "llm", 0.4, spanIDFor(1)), item("Faithfulness", "llm", 0.6, spanIDFor(2)),
	}})
	publishOK(t, w, sign(t, rk, runKID, validRunClaims()), first)
	// Publishing the same request again is a no-op.
	publishOK(t, w, sign(t, rk, runKID, validRunClaims()), first)
	if n := countRows(t, admin, "SELECT count(*) FROM span_scores"); n != 3 {
		t.Fatalf("span rows after a retry = %d, want 3", n)
	}

	secondRun := "9e8d7c6b-5a49-4382-a1b0-c9d8e7f6a5b4"
	claims := validRunClaims()
	secondIat := testNow.Add(-10 * time.Second)
	claims["runId"], claims["jti"], claims["iat"], claims["nbf"] = secondRun, secondRun, secondIat.Unix(), secondIat.Unix()
	second := mustJSON(t, map[string]any{"context": map[string]any{"monitorName": "Quality v2"}, "individualScores": []map[string]any{
		item("Faithfulness", "llm", 1.0, spanIDFor(7)),
	}})
	rec := doPublish(t, w, publishPath(testMonitorID, secondRun), sign(t, rk, runKID, jwt.MapClaims(claims)), second)
	if rec.Code != http.StatusCreated {
		t.Fatalf("second run status = %d, body %s", rec.Code, rec.Body)
	}

	faith := readTraceRow(t, admin, "Faithfulness", traceIDFor(0))
	if faith.RunID != secondRun || !faith.RunStartedAt.Equal(secondIat) || faith.MonitorName != "Quality v2" ||
		*faith.Score != 1.0 || faith.Scored != 1 {
		t.Errorf("re-run row = %+v, want the second run's", faith)
	}
	if n := countRows(t, admin, "SELECT count(*) FROM span_scores WHERE evaluator_name = 'Faithfulness'"); n != 1 {
		t.Errorf("Faithfulness span rows = %d, want only the re-run's 1", n)
	}
	latency := readTraceRow(t, admin, "Latency", traceIDFor(0))
	if latency.RunID != testRunID || *latency.Score != 0.9 {
		t.Errorf("other evaluator's row = %+v, want it untouched", latency)
	}
}

func TestStore_Speed500Traces(t *testing.T) {
	_, writerPool := testDatabases(t)
	w, rk := dbWriter(t, writerPool)
	token := sign(t, rk, runKID, validRunClaims())
	body := speedBody(t)
	srv := httptest.NewTLSServer(w.publishHandler())
	t.Cleanup(srv.Close)

	for run := 0; run < 3; run++ {
		req, err := http.NewRequest(http.MethodPost, srv.URL+publishPath(testMonitorID, testRunID), bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		begin := time.Now()
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		t.Logf("500 traces x 3 evaluators x 5 spans (%d KiB) over HTTPS: %s", len(body)/1024, time.Since(begin))
	}
}

// speedBody is 500 traces scored by 3 llm-level evaluators over 5 spans each.
func speedBody(t *testing.T) []byte {
	t.Helper()
	start := testNow.Add(-time.Hour).Format(time.RFC3339Nano)
	items := make([]map[string]any, 0, 500*3*5)
	for i := 0; i < 500; i++ {
		for _, evaluator := range []string{"Faithfulness", "Relevance", "Toxicity"} {
			for s := 0; s < 5; s++ {
				items = append(items, map[string]any{
					"evaluatorName": evaluator, "level": "llm", "traceId": traceIDFor(i), "traceStartTime": start,
					"score": float64(s) / 4, "explanation": "The answer is grounded in the retrieved context.",
					"spanContext": map[string]any{"spanId": spanIDFor(s), "model": "gpt-4o", "vendor": "openai"},
				})
			}
		}
	}
	return mustJSON(t, map[string]any{"context": map[string]any{"monitorName": "Quality"}, "individualScores": items})
}
