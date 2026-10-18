-- One row per (monitor, evaluator, trace); a re-run replaces the row and its span rows.
CREATE TABLE trace_scores (
    org_id                text             NOT NULL CHECK (org_id <> ''),
    monitor_id            uuid             NOT NULL,
    evaluator_name        text             NOT NULL,
    trace_id              text             NOT NULL,
    project_name          text             NOT NULL,
    agent_name            text             NOT NULL,
    environment_name      text             NOT NULL,
    monitor_name          text             NOT NULL,
    run_id                uuid             NOT NULL,
    run_started_at        timestamptz      NOT NULL,
    evaluator_identifier  text             NOT NULL,
    level                 text             NOT NULL CHECK (level IN ('trace', 'agent', 'llm')),
    trace_start_time      timestamptz      NOT NULL,
    score                 numeric(5,4)     CHECK (score BETWEEN 0 AND 1),
    score_sum             double precision NOT NULL DEFAULT 0,
    scored_count          integer          NOT NULL,
    skipped_count         integer          NOT NULL,
    skip_reason           text,
    explanation           text,
    trace_status          text,
    trace_duration_ns     bigint,
    trace_span_count      integer,
    trace_total_tokens    bigint,
    trace_models          text[],
    trace_tools           text[],
    trace_failed_tools    text[],
    trace_mcp_servers     text[],
    trace_conversation_id text,
    published_at          timestamptz      NOT NULL DEFAULT now(),
    PRIMARY KEY (org_id, monitor_id, evaluator_name, trace_id)
);

-- Scored spans of agent and llm evaluators; removed with their trace row.
CREATE TABLE span_scores (
    org_id          text         NOT NULL CHECK (org_id <> ''),
    monitor_id      uuid         NOT NULL,
    evaluator_name  text         NOT NULL,
    trace_id        text         NOT NULL,
    span_id         text         NOT NULL,
    label           text         NOT NULL DEFAULT '',
    score           numeric(5,4) CHECK (score BETWEEN 0 AND 1),
    skip_reason     text,
    explanation     text,
    PRIMARY KEY (org_id, monitor_id, evaluator_name, trace_id, span_id),
    FOREIGN KEY (org_id, monitor_id, evaluator_name, trace_id)
        REFERENCES trace_scores (org_id, monitor_id, evaluator_name, trace_id) ON DELETE CASCADE
);
