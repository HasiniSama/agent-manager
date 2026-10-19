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
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// publishBatch is one publish request's rows and the run token's identity.
type publishBatch struct {
	OrgID           string
	MonitorID       string
	RunID           string
	ProjectName     string
	AgentName       string
	EnvironmentName string
	MonitorName     string
	RunStartedAt    time.Time
	Traces          []traceRow
	Spans           []spanRow
}

// scoreStore writes one publish request's rows.
type scoreStore interface {
	publish(ctx context.Context, b *publishBatch) error
}

// deleteFilter selects one org's rows for some monitors, or one evaluator's rows from runs started before a time.
type deleteFilter struct {
	OrgID            string
	MonitorIDs       []string
	EvaluatorName    *string
	RunStartedBefore *time.Time
}

// scoreDeleter deletes score rows and reports how many trace rows went.
type scoreDeleter interface {
	deleteScores(ctx context.Context, f *deleteFilter) (int64, error)
}

// store writes score rows as the writer login.
type store struct {
	pool *pgxpool.Pool
}

const upsertTraceScoresSQL = `
INSERT INTO trace_scores (
    org_id, monitor_id, evaluator_name, trace_id, project_name, agent_name, environment_name,
    monitor_name, run_id, run_started_at, evaluator_identifier, level, trace_start_time,
    score, score_sum, scored_count, skipped_count, skip_reason, explanation, published_at)
SELECT $1, $2::uuid, t.evaluator_name, t.trace_id, $3, $4, $5, $6, $7::uuid, $8,
       t.evaluator_identifier, t.level, t.trace_start_time,
       t.score, t.score_sum, t.scored_count, t.skipped_count, t.skip_reason, t.explanation, now()
FROM unnest($9::text[], $10::text[], $11::text[], $12::text[], $13::timestamptz[],
            $14::float8[], $15::float8[], $16::int4[], $17::int4[], $18::text[], $19::text[])
     AS t(evaluator_name, trace_id, evaluator_identifier, level, trace_start_time,
          score, score_sum, scored_count, skipped_count, skip_reason, explanation)
ON CONFLICT (org_id, monitor_id, evaluator_name, trace_id) DO UPDATE SET
    project_name         = EXCLUDED.project_name,
    agent_name           = EXCLUDED.agent_name,
    environment_name     = EXCLUDED.environment_name,
    monitor_name         = EXCLUDED.monitor_name,
    run_id               = EXCLUDED.run_id,
    run_started_at       = EXCLUDED.run_started_at,
    evaluator_identifier = EXCLUDED.evaluator_identifier,
    level                = EXCLUDED.level,
    trace_start_time     = EXCLUDED.trace_start_time,
    score                = EXCLUDED.score,
    score_sum            = EXCLUDED.score_sum,
    scored_count         = EXCLUDED.scored_count,
    skipped_count        = EXCLUDED.skipped_count,
    skip_reason          = EXCLUDED.skip_reason,
    explanation          = EXCLUDED.explanation,
    published_at         = EXCLUDED.published_at`

const deleteSpanScoresSQL = `
DELETE FROM span_scores s
USING unnest($3::text[], $4::text[]) AS k(evaluator_name, trace_id)
WHERE s.org_id = $1 AND s.monitor_id = $2::uuid
  AND s.evaluator_name = k.evaluator_name AND s.trace_id = k.trace_id`

const insertSpanScoresSQL = `
INSERT INTO span_scores (
    org_id, monitor_id, evaluator_name, trace_id, span_id, label, score, skip_reason, explanation)
SELECT $1, $2::uuid, s.evaluator_name, s.trace_id, s.span_id, s.label, s.score, s.skip_reason, s.explanation
FROM unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::float8[], $8::text[], $9::text[])
     AS s(evaluator_name, trace_id, span_id, label, score, skip_reason, explanation)`

// The outer run_started_at check holds for a row a concurrent publish replaced after the batch was picked.
const deleteTraceScoresSQL = `
DELETE FROM trace_scores t
USING (
    SELECT monitor_id, evaluator_name, trace_id FROM trace_scores
    WHERE org_id = $1 AND monitor_id = ANY($2::text[]::uuid[])
      AND ($3::text IS NULL OR (evaluator_name = $3::text AND run_started_at < $4::timestamptz))
    LIMIT $5) d
WHERE t.org_id = $1 AND t.monitor_id = d.monitor_id
  AND t.evaluator_name = d.evaluator_name AND t.trace_id = d.trace_id
  AND ($3::text IS NULL OR t.run_started_at < $4::timestamptz)`

const deleteBatchSize = 5000

// deleteScores deletes matching trace rows in batches, each its own transaction; span rows go with them by foreign key.
func (s *store) deleteScores(ctx context.Context, f *deleteFilter) (int64, error) {
	var total int64
	for {
		tag, err := s.pool.Exec(ctx, deleteTraceScoresSQL, f.OrgID, f.MonitorIDs, f.EvaluatorName, f.RunStartedBefore, deleteBatchSize)
		if err != nil {
			return total, fmt.Errorf("delete trace scores: %w", err)
		}
		total += tag.RowsAffected()
		if tag.RowsAffected() < deleteBatchSize {
			return total, nil
		}
	}
}

// publish upserts the trace rows and replaces their span rows in one transaction, so a retry is safe.
func (s *store) publish(ctx context.Context, b *publishBatch) error {
	n := len(b.Traces)
	var (
		evaluators  = make([]string, n)
		traceIDs    = make([]string, n)
		identifiers = make([]string, n)
		levels      = make([]string, n)
		starts      = make([]time.Time, n)
		scores      = make([]*float64, n)
		sums        = make([]float64, n)
		scored      = make([]int32, n)
		skipped     = make([]int32, n)
		skipReasons = make([]*string, n)
		explains    = make([]*string, n)
	)
	for i, t := range b.Traces {
		evaluators[i], traceIDs[i], identifiers[i], levels[i] = t.EvaluatorName, t.TraceID, t.EvaluatorIdentifier, t.Level
		starts[i], scores[i], sums[i] = t.TraceStartTime, t.Score, t.ScoreSum
		scored[i], skipped[i], skipReasons[i], explains[i] = t.ScoredCount, t.SkippedCount, t.SkipReason, t.Explanation
	}

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, upsertTraceScoresSQL,
			b.OrgID, b.MonitorID, b.ProjectName, b.AgentName, b.EnvironmentName, b.MonitorName, b.RunID, b.RunStartedAt,
			evaluators, traceIDs, identifiers, levels, starts, scores, sums, scored, skipped, skipReasons, explains,
		); err != nil {
			return fmt.Errorf("upsert trace scores: %w", err)
		}
		if _, err := tx.Exec(ctx, deleteSpanScoresSQL, b.OrgID, b.MonitorID, evaluators, traceIDs); err != nil {
			return fmt.Errorf("delete span scores: %w", err)
		}
		if len(b.Spans) == 0 {
			return nil
		}
		m := len(b.Spans)
		var (
			spanEvaluators = make([]string, m)
			spanTraceIDs   = make([]string, m)
			spanIDs        = make([]string, m)
			labels         = make([]string, m)
			spanScores     = make([]*float64, m)
			spanSkips      = make([]*string, m)
			spanExplains   = make([]*string, m)
		)
		for i, sp := range b.Spans {
			spanEvaluators[i], spanTraceIDs[i], spanIDs[i], labels[i] = sp.EvaluatorName, sp.TraceID, sp.SpanID, sp.Label
			spanScores[i], spanSkips[i], spanExplains[i] = sp.Score, sp.SkipReason, sp.Explanation
		}
		if _, err := tx.Exec(ctx, insertSpanScoresSQL, b.OrgID, b.MonitorID,
			spanEvaluators, spanTraceIDs, spanIDs, labels, spanScores, spanSkips, spanExplains,
		); err != nil {
			return fmt.Errorf("insert span scores: %w", err)
		}
		return nil
	})
}
