-- Trace list: one agent and environment, newest first, keyset paging.
CREATE INDEX trace_scores_list ON trace_scores
    (org_id, project_name, agent_name, environment_name, trace_start_time DESC, trace_id DESC)
    INCLUDE (evaluator_name, score, score_sum, scored_count);

-- Scores for a page of trace IDs, and one trace's scores.
CREATE INDEX trace_scores_by_trace ON trace_scores (org_id, trace_id);

-- Dashboards: one monitor over a time range.
CREATE INDEX trace_scores_monitor_time ON trace_scores (monitor_id, trace_start_time);

-- Retention.
CREATE INDEX trace_scores_start ON trace_scores (trace_start_time);

-- Deletes by (org_id, monitor_id) use the primary key's leading columns, so they need no index of their own.
