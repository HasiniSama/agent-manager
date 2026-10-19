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
	"context"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// seedScores stores one evaluator's rows for traces [from, to), each with one span, through the writer login.
func seedScores(t *testing.T, st *store, org, monitorID, evaluator string, runStartedAt time.Time, from, to int) {
	t.Helper()
	b := &publishBatch{
		OrgID: org, MonitorID: monitorID, RunID: testRunID, ProjectName: "proj", AgentName: "agent",
		EnvironmentName: "dev", MonitorName: "Quality", RunStartedAt: runStartedAt,
	}
	score := 0.5
	for i := from; i < to; i++ {
		b.Traces = append(b.Traces, traceRow{
			EvaluatorName: evaluator, EvaluatorIdentifier: evaluator, Level: "llm", TraceID: traceIDFor(i),
			TraceStartTime: testNow.Add(-time.Hour), Score: &score, ScoreSum: score, ScoredCount: 1,
		})
		b.Spans = append(b.Spans, spanRow{EvaluatorName: evaluator, TraceID: traceIDFor(i), SpanID: spanIDFor(0), Label: "gpt-4o", Score: &score})
	}
	if err := st.publish(context.Background(), b); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// scoreCounts is the number of trace and span rows for one org, monitor and evaluator.
func scoreCounts(t *testing.T, admin *pgxpool.Pool, org, monitorID, evaluator string) (traces, spans int) {
	t.Helper()
	traces = countRows(t, admin, `SELECT count(*) FROM trace_scores WHERE org_id = $1 AND monitor_id = $2 AND evaluator_name = $3`, org, monitorID, evaluator)
	spans = countRows(t, admin, `SELECT count(*) FROM span_scores WHERE org_id = $1 AND monitor_id = $2 AND evaluator_name = $3`, org, monitorID, evaluator)
	return traces, spans
}

func TestStore_DeleteByMonitorKeepsOtherOrgsAndMonitors(t *testing.T) {
	admin, writerPool := testDatabases(t)
	st := &store{pool: writerPool}
	iat := testNow.Add(-time.Minute)
	seedScores(t, st, "org-1", testMonitorID, "Latency", iat, 0, 2)
	seedScores(t, st, "org-1", otherMonitorID, "Latency", iat, 0, 2)
	seedScores(t, st, "org-2", testMonitorID, "Latency", iat, 0, 2)

	for attempt, want := range []int64{2, 0} {
		n, err := st.deleteScores(context.Background(), &deleteFilter{OrgID: "org-1", MonitorIDs: []string{testMonitorID}})
		if err != nil {
			t.Fatalf("delete %d: %v", attempt, err)
		}
		if n != want {
			t.Errorf("delete %d removed %d trace rows, want %d", attempt, n, want)
		}
		if tr, sp := scoreCounts(t, admin, "org-1", testMonitorID, "Latency"); tr != 0 || sp != 0 {
			t.Errorf("delete %d: org-1's monitor has %d trace and %d span rows, want none", attempt, tr, sp)
		}
		if tr, sp := scoreCounts(t, admin, "org-1", otherMonitorID, "Latency"); tr != 2 || sp != 2 {
			t.Errorf("delete %d: org-1's other monitor has %d trace and %d span rows, want 2 and 2", attempt, tr, sp)
		}
		if tr, sp := scoreCounts(t, admin, "org-2", testMonitorID, "Latency"); tr != 2 || sp != 2 {
			t.Errorf("delete %d: org-2's rows for the same monitor ID are %d and %d, want 2 and 2", attempt, tr, sp)
		}
	}
}

func TestStore_DeleteEvaluatorKeepsLaterRunsAndOtherEvaluators(t *testing.T) {
	admin, writerPool := testDatabases(t)
	st := &store{pool: writerPool}
	cutoff := testNow.Add(-time.Hour)
	seedScores(t, st, "org-1", testMonitorID, "Latency", cutoff.Add(-time.Hour), 0, 2)
	seedScores(t, st, "org-1", testMonitorID, "Latency", cutoff.Add(time.Minute), 2, 4)
	seedScores(t, st, "org-1", testMonitorID, "Toxicity", cutoff.Add(-time.Hour), 0, 2)
	seedScores(t, st, "org-2", testMonitorID, "Latency", cutoff.Add(-time.Hour), 0, 2)
	evaluator := "Latency"

	for attempt, want := range []int64{2, 0} {
		n, err := st.deleteScores(context.Background(), &deleteFilter{
			OrgID: "org-1", MonitorIDs: []string{testMonitorID}, EvaluatorName: &evaluator, RunStartedBefore: &cutoff,
		})
		if err != nil {
			t.Fatalf("delete %d: %v", attempt, err)
		}
		if n != want {
			t.Errorf("delete %d removed %d trace rows, want %d", attempt, n, want)
		}
		if tr, sp := scoreCounts(t, admin, "org-1", testMonitorID, "Latency"); tr != 2 || sp != 2 {
			t.Errorf("delete %d: Latency has %d trace and %d span rows, want the later run's 2 and 2", attempt, tr, sp)
		}
		if n := countRows(t, admin, `SELECT count(*) FROM trace_scores WHERE org_id = 'org-1' AND evaluator_name = 'Latency' AND run_started_at < $1`, cutoff); n != 0 {
			t.Errorf("delete %d: %d Latency rows from before the cut-off remain", attempt, n)
		}
		if tr, sp := scoreCounts(t, admin, "org-1", testMonitorID, "Toxicity"); tr != 2 || sp != 2 {
			t.Errorf("delete %d: Toxicity has %d and %d rows, want 2 and 2", attempt, tr, sp)
		}
		if tr, sp := scoreCounts(t, admin, "org-2", testMonitorID, "Latency"); tr != 2 || sp != 2 {
			t.Errorf("delete %d: org-2 has %d and %d rows, want 2 and 2", attempt, tr, sp)
		}
	}
}

func TestStore_DeleteInBatches(t *testing.T) {
	admin, writerPool := testDatabases(t)
	st := &store{pool: writerPool}
	seedScores(t, st, "org-1", testMonitorID, "Latency", testNow.Add(-time.Minute), 0, deleteBatchSize+1)

	n, err := st.deleteScores(context.Background(), &deleteFilter{OrgID: "org-1", MonitorIDs: []string{testMonitorID}})
	if err != nil {
		t.Fatal(err)
	}
	if n != deleteBatchSize+1 {
		t.Errorf("removed %d trace rows, want %d", n, deleteBatchSize+1)
	}
	if tr, sp := scoreCounts(t, admin, "org-1", testMonitorID, "Latency"); tr != 0 || sp != 0 {
		t.Errorf("%d trace and %d span rows remain, want none", tr, sp)
	}
}

// dumpScores lists every trace and span row as JSON, without published_at.
func dumpScores(t *testing.T, admin *pgxpool.Pool) []string {
	t.Helper()
	rows, err := admin.Query(context.Background(), `
		SELECT (to_jsonb(t) - 'published_at')::text FROM trace_scores t
		UNION ALL
		SELECT to_jsonb(s)::text FROM span_scores s
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestBackfill_WritesWhatPublishWrites(t *testing.T) {
	publishedAdmin, publishedPool := testDatabases(t)
	backfilledAdmin, backfilledPool := testDatabases(t)
	pw, rk := dbWriter(t, publishedPool)
	body := scoresBody(t, 2, 2)
	publishOK(t, pw, sign(t, rk, runKID, validRunClaims()), body)

	_, tk := testKeys(t)
	srv, _ := jwksServer(t, map[string]*rsa.PublicKey{thunderKID: &tk.PublicKey})
	backfilled := &store{pool: backfilledPool}
	bw := newInternalTestWriter(srv.URL, backfilled, backfilled, true)
	var published struct {
		IndividualScores []map[string]any `json:"individualScores"`
	}
	if err := json.Unmarshal(body, &published); err != nil {
		t.Fatal(err)
	}
	rec := doInternal(t, bw, backfillPath, sign(t, tk, thunderKID, validServiceClaims()), backfillBody(t, func(m map[string]any) {
		m["individualScores"] = published.IndividualScores
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("backfill status = %d, body %s", rec.Code, rec.Body)
	}

	want, got := dumpScores(t, publishedAdmin), dumpScores(t, backfilledAdmin)
	if len(want) == 0 || !slices.Equal(got, want) {
		t.Errorf("backfilled rows differ from published rows\n got: %v\nwant: %v", got, want)
	}
}
