# amp-evaluation — agent guide

Trace-based evaluation framework for AI agents. Reads OTel traces and scores them with rule-based or LLM-as-judge evaluators, in one of two runner modes. Packaged with **hatchling**; Python ≥ 3.10; models use **Pydantic v2**.

## Two runner modes

| Mode | Class | Input | When |
|---|---|---|---|
| **Experiment** | `Experiment` | dataset of `Task`s + an agent invoker | offline: invoke the agent per task, evaluate each resulting trace |
| **Monitor** | `Monitor` | evaluators + a `TraceFetcher` | live: fetch production traces over a time window, evaluate without tasks |

Both validate that evaluator names are unique and compute per-evaluator aggregations (mean, stddev, …).

## Layout (`src/amp_evaluation/`)

| Area | Path | Role |
|---|---|---|
| Public API | `__init__.py` | `Experiment`, `Monitor`, `@evaluator`, `@llm_judge`, `BaseEvaluator`, … |
| Runners | `runner.py` | `Experiment`, `Monitor`, `BaseRunner` |
| Evaluator base | `evaluators/base.py` | `BaseEvaluator`, `LLMAsJudgeEvaluator`, `FunctionEvaluator`, type-hint detection |
| Levels/modes | `evaluators/params.py` | `Param`, `EvaluationLevel` (TRACE/AGENT/LLM), `EvalMode` (EXPERIMENT/MONITOR) |
| Built-ins | `evaluators/builtin/` | `standard.py` (latency, hallucination…), `llm_judge.py` (16 judges), `deepeval.py` |
| Registry | `registry.py` | `@evaluator`, `@llm_judge`, `discover_evaluators()` |
| Dataset | `dataset/models.py`, `dataset/loader.py` | `Task`, `Dataset`, JSON/CSV load-save |
| Trace | `trace/models.py`, `trace/fetcher.py`, `trace/parser.py` | `Trace`/`AgentTrace`/`LLMSpan`, `/traces/export` fetch, parse |
| Results | `models.py` | `EvalResult`, `EvaluatorScore`, `EvaluatorSummary` |
| Config | `config.py` | Pydantic Settings, `AMP_*` env vars (org/project/agent/env IDs) |

## Defining an evaluator

Decorate a function. **The level and mode are auto-detected from type hints** — this is the key convention:

```python
from amp_evaluation import evaluator, Trace, Task, EvalResult

@evaluator("my-check", description="…", tags=["rule-based", "quality"])
def evaluate(trace: Trace) -> EvalResult:      # first-arg type → level
    ...
```

- **First parameter type** sets the level: `Trace` → TRACE, `AgentTrace` → AGENT, `LLMSpan` → LLM.
- A required `task: Task` param makes it **EXPERIMENT-only**; `task: Optional[Task] = None` makes it work in **both** modes.
- LLM-as-judge evaluators implement `build_prompt()` instead of `evaluate()`, with the same level/mode detection. Tag them `["llm-judge", <aspect>]`.
- Configurable knobs use the `Param` descriptor: `max_latency_ms: float = Param(default=5000, description="…")`.
- Names **must be unique** (enforced in `runner.run()`).

`discover_evaluators(module)` scans a module for every `BaseEvaluator` instance.

## Commands

No Makefile. From this dir:
```bash
pip install -e '.[dev]'        # dev extras: pytest, pytest-cov, black, ruff, deepeval, any-llm
pytest                         # runs with -v --cov=amp_evaluation (see pyproject)
ruff check src/                # lint
black src/                     # format (project configures Black; don't also run `ruff format`)
mypy src/                      # type-check (excludes samples/)
```

Config: **ruff/black** line-length 120; **mypy** ignores missing imports for deepeval/any_llm/boto3/httpx. Tests in `tests/`, coverage on by default.

## Optional dependencies

- `deepeval` extra → DeepEval-backed evaluators.
- `any-llm` extra → LLM-as-judge backends (`any-llm-sdk` with openai/anthropic/gemini/… providers).

## Gotchas

- Type-hint detection uses `typing.get_type_hints()` and falls back to `inspect` on failure — keep annotations importable.
- `RequestsInstrumentor` is initialized once at module level (global guard) to avoid double instrumentation.
- LLM-judge built-ins need LLM config via the `any-llm` extra; `semantic_similarity` needs `expected_output` on the task.
- The dataset loader auto-generates task IDs when missing.
- `parse_trace_for_evaluation` drops evaluation spans (a non-empty `amp.eval.run.id`, written by monitor runs) before building any view. Spans with `gen_ai.evaluation.*` but no run ID are kept.

Consumed as a dependency by `../../evaluation-job/AGENTS.md` (the K8s job that runs Monitor evaluations).
