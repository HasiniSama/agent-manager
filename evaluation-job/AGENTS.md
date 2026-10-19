# evaluation-job — agent guide

A Dockerized Python **job** (not a library) that runs Monitor-mode evaluations against agent traces in a time window. Invoked by an Argo `ClusterWorkflowTemplate`. It wraps `amp-evaluation`'s `Monitor` runner, fetches traces from the traces API, and publishes scores back to the platform.

## What it is (and isn't)

- Two files: `main.py` (the job) and `test_main.py`.
- `pyproject.toml` has **no `[project]` section, no dependencies, no build backend** — only ruff/mypy/pytest config. This is intentional: it is never published to PyPI; deps come from the Docker image (which installs `amp-evaluation`).
- Entry point is `python main.py` with argparse CLI flags.

## `main.py` structure

- `OAuth2TokenManager` — client-credentials token caching (refresh when within 30s of expiry).
- `handle_sigterm` → logs and exits immediately via `sys.exit(0)` (graceful shutdown for Kubernetes). It does not set a shutdown flag, and nothing polls one.
- Argparse config validation → evaluator registration from a JSON arg → trace fetch (paginated) → `Monitor.run()` → publish scores.

### CLI flags

| Flag | Notes |
|---|---|
| `--monitor-name`, `--monitor-id`, `--run-id` | monitor + run identity |
| `--organization`, `--project`, `--agent`, `--environment` | scope of the traces to evaluate |
| `--evaluators='[{…}]'` | JSON array of `{name, …config}`; each becomes a builtin evaluator via `builtin(name, **config)` |
| `--sampling-rate` | float, default 1.0 |
| `--trace-start`, `--trace-end` | ISO-8601 window |
| `--traces-api-endpoint` | e.g. `http://amp-observer:8080` |
| `--publisher-endpoint` | agent-manager internal API base for score publishing (e.g. `http://agent-manager-internal:8081`) |
| `--score-writer-endpoint`, `--score-writer-ca` | optional https base of `amp-score-writer` and the CA file that verifies it; empty skips the second publish |

OAuth2 client credentials for publisher authentication are read from the environment (via `OAuth2TokenManager`), not passed as CLI flags.

Traces are fetched via `TraceFetcher.fetch_traces(...)`. Page size is configurable via the `page_size` parameter and defaults to the value set on the `TraceFetcher` instance; this job constructs the fetcher with `page_size=TRACE_FETCH_PAGE_SIZE` (a module constant, currently `10`, to bound memory). Scores POST to `/api/v1/publisher/monitors/{monitor_id}/runs/{run_id}/scores` (built from `--publisher-endpoint`) as an `EvaluatorSummary`-derived payload (`individualScores` + `aggregatedScores`), with exponential backoff (3 retries, 2s initial).

With `--score-writer-endpoint` set and the run token file at `EVAL_RUN_TOKEN_FILE`, the job then posts the same per-trace scores (plus `evaluatorIdentifier`) to the score writer's route of the same path, with the run token as the Bearer, in batches of at most 500 traces that keep each trace's items together. Results for traces the run did not fetch, or for evaluators not in `--evaluators`, are dropped first and counted. A writer failure is logged and never changes the exit code; without the token file the publish is skipped with one warning.

## Commands

```bash
make lint             # ruff check .
make format-check     # ruff format --check .
make type-check       # mypy main.py --install-types --non-interactive
make test             # pytest test_main.py -v
make docker-build     # image installing amp-evaluation from PyPI (AMP_EVALUATION_VERSION arg)
make docker-build-dev # image installing amp-evaluation from local ../libs (PROJECT_ROOT=..)
make docker-load-k3d  # build-dev + load into k3d
```

Config: ruff/mypy target **py311**, line-length 120.

## Gotchas

- The `EvaluatorSummary` JSON published must match the Pydantic schema in `amp-evaluation` exactly.
- Most runtime config comes from `AMP_*` env vars consumed by `amp-evaluation.Config`, not from CLI flags.
- Page size (10) is not a flag — change it in the source if needed.
- For local iteration on the evaluator code, use `make docker-build-dev` so it picks up your local `../libs/amp-evaluation` instead of the published version.

Depends on `../libs/amp-evaluation/AGENTS.md` (evaluators + `Monitor`) and hits `../agent-manager-observer/AGENTS.md` for traces.
