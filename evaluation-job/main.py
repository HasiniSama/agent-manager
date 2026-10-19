# Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
#
# WSO2 LLC. licenses this file to you under the Apache License,
# Version 2.0 (the "License"); you may not use this file except
# in compliance with the License.
# You may obtain a copy of the License at
#
# http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.

"""
Monitor job for running evaluations in Argo Workflows.

This script is invoked by the ClusterWorkflowTemplate to run monitor evaluations
against agent traces within a specified time window.

Uses the amp-evaluation SDK to register evaluators and run the monitor.

Usage:
    python main.py \
        --monitor-name=my-monitor \
        --agent-id=agent-uid-123 \
        --environment-id=env-uid-456 \
        --evaluators='[{"name":"latency_performance","config":{"max_latency_ms":3000}}]' \
        --sampling-rate=1.0 \
        --trace-start=2026-01-01T00:00:00Z \
        --trace-end=2026-01-02T00:00:00Z \
        --traces-api-endpoint=http://amp-observer:8080
"""

import argparse
import json
import logging
import os
import signal
import sys
import time
from datetime import datetime
from typing import Dict, Iterator, List, Any, cast

import requests
from amp_evaluation import Monitor, builtin
from amp_evaluation.models import EvaluatorScore, EvaluatorSummary
from amp_evaluation.trace import TraceFetcher
from amp_evaluation.trace.fetcher import OTELTrace

logger = logging.getLogger(__name__)

# Maximum number of retries for publishing scores
PUBLISH_MAX_RETRIES = 3
PUBLISH_INITIAL_BACKOFF = 2  # seconds

# Most traces in one score-writer request; the writer rejects more.
WRITER_BATCH_TRACES = 500
# A score-writer batch closes before its body passes this, under the writer's 8 MiB limit.
WRITER_BATCH_BYTES = 6 * 1024 * 1024

# Max traces per /traces/export page. Kept small and deliberate -- each page is fully
# parsed into memory at once, so this bounds peak memory regardless of how many traces
# match the monitor's time window. Not exposed as a CLI flag; tune this constant directly.
TRACE_FETCH_PAGE_SIZE = 10


class OAuth2TokenManager:
    """Manages OAuth2 client_credentials tokens with caching."""

    def __init__(self, token_url: str, client_id: str, client_secret: str):
        self.token_url = token_url
        self.client_id = client_id
        self.client_secret = client_secret
        self._token: str | None = None
        self._expires_at: float = 0

    def get_token(self) -> str:
        """Get a valid access token, refreshing if needed."""
        if self._token and time.time() < self._expires_at - 30:
            return self._token

        # Use client_secret_basic: credentials in Authorization header (Base64 encoded)
        import base64

        auth = base64.b64encode(f"{self.client_id}:{self.client_secret}".encode()).decode()
        response = requests.post(
            self.token_url,
            data={"grant_type": "client_credentials"},
            headers={"Authorization": f"Basic {auth}"},
            timeout=10,
        )
        try:
            response.raise_for_status()
        except requests.HTTPError as e:
            status = e.response.status_code if e.response is not None else "unknown"
            raise requests.HTTPError(f"Token request failed: HTTP {status}") from None
        data = response.json()
        self._token = data["access_token"]
        self._expires_at = time.time() + data.get("expires_in", 3600)
        return self._token


class FetchedTraceRecorder:
    """Passes a TraceFetcher's traces through and remembers each trace ID."""

    def __init__(self, fetcher: TraceFetcher):
        self._fetcher = fetcher
        self.trace_ids: set[str] = set()

    def fetch_traces(self, **kwargs: Any) -> Iterator[OTELTrace]:
        for trace in self._fetcher.fetch_traces(**kwargs):
            self.trace_ids.add(trace.traceId)
            yield trace


def read_run_token() -> str | None:
    """Return the run token in EVAL_RUN_TOKEN_FILE, or None when unset, missing or empty."""
    path = os.environ.get("EVAL_RUN_TOKEN_FILE")
    if not path:
        return None
    try:
        with open(path, encoding="utf-8") as f:
            token = f.read().strip()
    except OSError:
        return None
    return token or None


def handle_sigterm(signum: int, frame: Any) -> None:
    """Handle SIGTERM for graceful shutdown in Kubernetes."""
    logger.info("Received SIGTERM, shutting down gracefully")
    sys.exit(0)


signal.signal(signal.SIGTERM, handle_sigterm)


class JsonFormatter(logging.Formatter):
    """Format log records as single-line JSON matching Go slog output."""

    # Python uses "WARNING"; normalise to the short form used by Go slog.
    _LEVEL_MAP = {"WARNING": "WARN"}

    def format(self, record):
        log_entry = {
            "time": self.formatTime(record, self.datefmt),
            "level": self._LEVEL_MAP.get(record.levelname, record.levelname),
            "msg": record.getMessage(),
            "logger": record.name,
        }
        if record.exc_info and record.exc_info[0] is not None:
            log_entry["trace"] = self.formatException(record.exc_info)
        if record.stack_info:
            log_entry["stack"] = self.formatStack(record.stack_info)
        return json.dumps(log_entry)


def configure_logging() -> None:
    """Configure JSON logging from LOG_LEVEL env var (default: INFO).

    Logs are read by agent developers who cannot see the monitor/eval source,
    so INFO is reserved for run progress and per-evaluator summaries, DEBUG for
    monitor/eval internals, and WARN/ERROR for things the developer can act on
    (LLM misconfiguration, skipped evals, parse failures).

    LOG_LEVEL is applied to both the evaluation-job logger and the
    ``amp_evaluation`` library so DEBUG is actually usable in dev; the root
    logger is kept at INFO so noisy third-party libraries stay quiet.
    """
    level_name = os.environ.get("LOG_LEVEL", "INFO").upper()
    level = getattr(logging, level_name, logging.INFO)
    handler = logging.StreamHandler()
    handler.setFormatter(JsonFormatter(datefmt="%Y-%m-%dT%H:%M:%S"))
    logging.basicConfig(level=logging.INFO, handlers=[handler])
    logging.getLogger(__name__).setLevel(level)
    logging.getLogger("amp_evaluation").setLevel(level)

    # The openai/anthropic SDKs (used internally by any_llm) log retry attempts at
    # INFO via their own package loggers, not through evaluation-job's logger above.
    logging.getLogger("openai").setLevel(logging.WARNING)
    logging.getLogger("anthropic").setLevel(logging.WARNING)
    logging.getLogger("httpx").setLevel(logging.WARNING)


def parse_args() -> argparse.Namespace:
    """Parse command-line arguments for monitor execution."""
    parser = argparse.ArgumentParser(
        description="Run monitor evaluation for AI agent traces",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )

    parser.add_argument(
        "--monitor-name",
        required=True,
        help="Unique name of the monitor",
    )

    parser.add_argument(
        "--organization",
        required=True,
        help="Organisation name",
    )

    parser.add_argument(
        "--project",
        required=True,
        help="Project name",
    )

    parser.add_argument(
        "--agent",
        required=True,
        help="Agent name",
    )

    parser.add_argument(
        "--environment",
        required=True,
        help="Environment name",
    )

    parser.add_argument(
        "--evaluators",
        required=True,
        help='JSON array of evaluator configurations (e.g., \'[{"name":"latency_performance","config":{"max_latency_ms":3000}}]\')',
    )

    parser.add_argument(
        "--sampling-rate",
        type=float,
        default=1.0,
        help="Sampling rate for traces (> 0 and <= 1), default: 1.0",
    )

    parser.add_argument(
        "--trace-start",
        required=True,
        help="Start time for trace evaluation (ISO 8601 format)",
    )

    parser.add_argument(
        "--trace-end",
        required=True,
        help="End time for trace evaluation (ISO 8601 format)",
    )

    parser.add_argument(
        "--traces-api-endpoint",
        required=True,
        help="Traces API endpoint (e.g., http://amp-observer:8080)",
    )

    parser.add_argument(
        "--monitor-id",
        required=True,
        help="Monitor UUID for publishing scores",
    )

    parser.add_argument(
        "--run-id",
        required=True,
        help="Run UUID for this evaluation execution",
    )

    parser.add_argument(
        "--publisher-endpoint",
        required=True,
        help="Publisher API endpoint for score publishing (e.g., http://agent-manager-internal:8081)",
    )

    parser.add_argument(
        "--score-writer-endpoint",
        default="",
        help="Score writer https URL that also receives the scores; empty skips that publish",
    )

    parser.add_argument(
        "--score-writer-ca",
        default="",
        help="CA certificate file that verifies the score writer; empty uses the system trust store",
    )

    return parser.parse_args()


def validate_time_format(time_str: str) -> bool:
    """Validate ISO 8601 time format."""
    try:
        datetime.fromisoformat(time_str.replace("Z", "+00:00"))
        return True
    except ValueError:
        return False


def publish_scores(
    monitor_id: str,
    run_id: str,
    scores: Dict[str, EvaluatorSummary],
    display_name_to_identifier: Dict[str, str],
    api_endpoint: str,
    token_manager: OAuth2TokenManager,
) -> bool:
    """
    Publish evaluation scores to the agent-manager internal API.

    Args:
        monitor_id: Monitor UUID
        run_id: Run UUID
        scores: Dict of evaluator display_name -> EvaluatorSummary from RunResult
        display_name_to_identifier: Mapping of display_name -> evaluator identifier
        api_endpoint: Agent Manager internal API base URL
        token_manager: OAuth2 token manager for authentication

    Returns:
        True if publishing succeeded, False otherwise
    """
    if not scores:
        logger.warning("No scores to publish")
        return True

    # Build the publish request payload
    individual_scores: List[Dict[str, Any]] = []
    aggregated_scores: List[Dict[str, Any]] = []

    for display_name, summary in scores.items():
        identifier = display_name_to_identifier.get(display_name, display_name)
        # Add aggregated scores (evaluator metadata + aggregations)
        aggregated_scores.append(
            {
                "identifier": identifier,
                "evaluatorName": display_name,
                "level": summary.level,
                "aggregations": summary.aggregated_scores,
                "count": summary.count,
                "skippedCount": summary.skipped_count,
            }
        )

        # Add individual scores (per-trace/span scores)
        for score in summary.individual_scores:
            individual_scores.append(_score_item(display_name, summary.level, score))

    payload = {
        "individualScores": individual_scores,
        "aggregatedScores": aggregated_scores,
    }

    # Publish scores to agent-manager API
    url = f"{api_endpoint}/api/v1/publisher/monitors/{monitor_id}/runs/{run_id}/scores"

    logger.debug(
        "Publishing scores monitor_id=%s run_id=%s evaluators=%d individual_scores=%d",
        monitor_id,
        run_id,
        len(scores),
        len(individual_scores),
    )

    for attempt in range(PUBLISH_MAX_RETRIES):
        try:
            headers = {
                "Authorization": f"Bearer {token_manager.get_token()}",
                "Content-Type": "application/json",
            }
            response = requests.post(url, json=payload, headers=headers, timeout=30)
            response.raise_for_status()

            logger.debug("Successfully published scores to agent-manager")
            return True

        except requests.exceptions.RequestException as e:
            logger.error(
                "Failed to publish scores (attempt %d/%d): %s",
                attempt + 1,
                PUBLISH_MAX_RETRIES,
                e,
            )
            if hasattr(e, "response") and e.response is not None:
                logger.error("Response status: %d", e.response.status_code)
                logger.error("Response body: %s", e.response.text[:500])
                # Don't retry on 4xx client errors
                if 400 <= e.response.status_code < 500:
                    return False
            if attempt < PUBLISH_MAX_RETRIES - 1:
                backoff = PUBLISH_INITIAL_BACKOFF * (2**attempt)
                logger.debug("Retrying in %d seconds...", backoff)
                time.sleep(backoff)

    return False


def _score_item(display_name: str, level: str, score: EvaluatorScore) -> Dict[str, Any]:
    """Build one individualScores item as agent-manager receives it."""
    item: Dict[str, Any] = {
        "evaluatorName": display_name,
        "level": level,
        "traceId": score.trace_id,
    }

    # Optional fields
    if score.is_successful and score.score is not None:
        item["score"] = score.score
    elif score.skip_reason:
        item["skipReason"] = score.skip_reason
    else:
        item["skipReason"] = "Evaluation did not produce a score"
    if score.explanation:
        item["explanation"] = score.explanation
    if score.trace_start_time:
        item["traceStartTime"] = score.trace_start_time.isoformat()
    # Span context (enriched by the framework)
    if score.span_context:
        span_ctx: Dict[str, str] = {}
        if score.span_context.span_id:
            span_ctx["spanId"] = score.span_context.span_id
        if score.span_context.agent_name:
            span_ctx["agentName"] = score.span_context.agent_name
        if score.span_context.model:
            span_ctx["model"] = score.span_context.model
        if score.span_context.vendor:
            span_ctx["vendor"] = score.span_context.vendor
        if span_ctx:
            item["spanContext"] = span_ctx
    return item


def writer_score_items(
    scores: Dict[str, EvaluatorSummary],
    display_name_to_identifier: Dict[str, str],
    fetched_trace_ids: set[str],
) -> List[Dict[str, Any]]:
    """Build the score writer's items, dropping results for traces not fetched or evaluators not configured."""
    items: List[Dict[str, Any]] = []
    dropped = 0
    for display_name, summary in scores.items():
        identifier = display_name_to_identifier.get(display_name)
        for score in summary.individual_scores:
            if identifier is None or score.trace_id not in fetched_trace_ids:
                dropped += 1
                continue
            item = _score_item(display_name, summary.level, score)
            item["evaluatorIdentifier"] = identifier
            items.append(item)
    if dropped:
        logger.warning(
            "Dropped %d score(s) for traces this run did not fetch or evaluators it was not configured with",
            dropped,
        )
    return items


def batch_by_trace(items: List[Dict[str, Any]]) -> List[List[Dict[str, Any]]]:
    """Split items into batches that keep each trace's items together, within the writer's trace and size limits."""
    by_trace: Dict[str, List[Dict[str, Any]]] = {}
    for item in items:
        by_trace.setdefault(item["traceId"], []).append(item)

    batches: List[List[Dict[str, Any]]] = []
    current: List[Dict[str, Any]] = []
    traces = size = 0
    for group in by_trace.values():
        group_size = len(json.dumps(group))
        if current and (traces == WRITER_BATCH_TRACES or size + group_size > WRITER_BATCH_BYTES):
            batches.append(current)
            current, traces, size = [], 0, 0
        current.extend(group)
        traces += 1
        size += group_size
    if current:
        batches.append(current)
    return batches


def _post_to_writer(url: str, body: Dict[str, Any], headers: Dict[str, str], verify: str | bool) -> bool:
    """POST one batch to the score writer with publish_scores' retries."""
    for attempt in range(PUBLISH_MAX_RETRIES):
        try:
            response = requests.post(url, json=body, headers=headers, timeout=30, verify=verify)
            response.raise_for_status()
            return True
        except requests.exceptions.RequestException as e:
            logger.error(
                "Failed to publish scores to the score writer (attempt %d/%d): %s",
                attempt + 1,
                PUBLISH_MAX_RETRIES,
                e,
            )
            if e.response is not None:
                logger.error("Score writer response status: %d", e.response.status_code)
                logger.error("Score writer response body: %s", e.response.text[:500])
                if 400 <= e.response.status_code < 500:
                    return False
            if attempt < PUBLISH_MAX_RETRIES - 1:
                time.sleep(PUBLISH_INITIAL_BACKOFF * (2**attempt))
    return False


def publish_trace_scores(
    endpoint: str,
    monitor_id: str,
    run_id: str,
    monitor_name: str,
    items: List[Dict[str, Any]],
    run_token: str,
    ca_path: str,
) -> bool:
    """Publish per-trace scores to the score writer in batches, stopping at the first batch that fails."""
    if not items:
        logger.warning("No scores to publish to the score writer")
        return True

    url = f"{endpoint.rstrip('/')}/api/v1/publisher/monitors/{monitor_id}/runs/{run_id}/scores"
    headers = {"Authorization": f"Bearer {run_token}", "Content-Type": "application/json"}
    verify: str | bool = ca_path or True
    batches = batch_by_trace(items)
    for number, batch in enumerate(batches, start=1):
        body = {"context": {"monitorName": monitor_name}, "individualScores": batch}
        if not _post_to_writer(url, body, headers, verify):
            logger.error(
                "Score writer %s did not accept batch %d/%d; later batches not sent",
                endpoint,
                number,
                len(batches),
            )
            return False
        logger.info(
            "Published to the score writer batch=%d/%d traces=%d scores=%d",
            number,
            len(batches),
            len({item["traceId"] for item in batch}),
            len(batch),
        )
    return True


def _load_custom_code_evaluator(identifier: str, source: str, config: dict):
    """Dynamically load a custom code evaluator from Python source.

    Supports both plain function style (preferred) and class-based style:

    - Plain function (preferred): the platform wraps it with FunctionEvaluator so
      that Param() defaults in the function signature are honoured as config fields.
    - Class-based (backward-compatible): must be a BaseEvaluator subclass.
    """
    from amp_evaluation import BaseEvaluator
    from amp_evaluation.evaluators.base import FunctionEvaluator

    if not source or not source.strip():
        raise ValueError(f"Custom evaluator '{identifier}' has empty source")

    namespace = {"__name__": f"custom_evaluator_{identifier}"}
    exec(source, namespace)  # noqa: S102

    found_func: Any = None
    found_cls: Any = None
    for obj in namespace.values():
        if isinstance(obj, FunctionEvaluator):
            # Already decorated with @evaluator — use as-is
            found_func = obj
            break
        if callable(obj) and not isinstance(obj, type) and not isinstance(obj, BaseEvaluator):
            found_func = obj  # plain function — will be wrapped below
        elif isinstance(obj, type) and issubclass(obj, BaseEvaluator) and obj is not BaseEvaluator:
            found_cls = obj

    if found_func is not None:
        if isinstance(found_func, FunctionEvaluator):
            instance = found_func
        else:
            instance = FunctionEvaluator(found_func, name=identifier)

        logger.debug(
            "Loaded custom code evaluator: %s (function=%s)", identifier, getattr(found_func, "__name__", identifier)
        )
        instance = instance.with_config(**config) if config else instance
        return instance

    if found_cls is not None:
        logger.debug("Loaded custom code evaluator: %s (class=%s)", identifier, found_cls.__name__)
        return found_cls(**config)

    raise ValueError(
        f"Custom evaluator '{identifier}' source must define a plain function "
        f"(with optional Param() defaults) or a BaseEvaluator subclass"
    )


def _eval_template(template: str, variables: dict) -> str:
    """Render a prompt template by evaluating it as a Python f-string.

    Uses the same trust model as custom code evaluators (which use ``exec()``).
    """
    try:
        # Escape any triple-quotes in the template to avoid breaking the f-string wrapper.
        safe = template.replace('"""', '\\"\\"\\"')
        result = eval(f'f"""{safe}"""', {"__builtins__": __builtins__}, variables)  # noqa: S307
        return result if result is not None else ""
    except Exception as e:
        raise ValueError(f"Failed to evaluate template: {e}")


def _get_llm_base_keys() -> frozenset:
    from amp_evaluation.evaluators.base import LLMAsJudgeEvaluator
    from amp_evaluation.evaluators.params import _ParamDescriptor

    return frozenset(name for name, val in vars(LLMAsJudgeEvaluator).items() if isinstance(val, _ParamDescriptor))


_LLM_BASE_KEYS = _get_llm_base_keys()


def _create_custom_llm_judge(identifier: str, prompt_template: str, level: str, config: dict):
    """Create an LLM-as-judge evaluator from a prompt template.

    The prompt template supports Python expressions inside ``{...}`` placeholders,
    evaluated as f-string expressions. The trace/agent/LLM span object is always
    available, along with any user-defined config params (i.e. params not in the
    base LLM config).

    Example template (agent level)::

        You are an expert {domain} evaluator.
        Agent: {agent_trace.agent_name or 'agent'}
        Input: {agent_trace.input}
        Output: {agent_trace.output}
        Steps: {len(agent_trace.steps)}

    Here ``domain`` would be a user-defined config param.
    """
    if not prompt_template or not prompt_template.strip():
        raise ValueError(f"Custom LLM-judge evaluator '{identifier}' has empty prompt template")

    from amp_evaluation.evaluators.base import FunctionLLMJudge
    from amp_evaluation.trace.models import Trace, AgentTrace, LLMSpan

    if level not in ("agent", "llm", "trace"):
        raise ValueError(f"Unsupported evaluator level: {level}")

    # User-defined params (non-LLM-base) are injected as extra template variables
    template_extra = {k: v for k, v in config.items() if k not in _LLM_BASE_KEYS}
    llm_config = {k: v for k, v in config.items() if k in _LLM_BASE_KEYS}

    if level == "agent":

        def _build_prompt(agent_trace: AgentTrace, task=None) -> str:
            return _eval_template(prompt_template, {"agent_trace": agent_trace, "task": task, **template_extra})
    elif level == "llm":

        def _build_prompt(llm_span: LLMSpan, task=None) -> str:  # type: ignore[misc]
            return _eval_template(prompt_template, {"llm_span": llm_span, "task": task, **template_extra})
    else:

        def _build_prompt(trace: Trace, task=None) -> str:  # type: ignore[misc]
            return _eval_template(prompt_template, {"trace": trace, "task": task, **template_extra})

    logger.debug("Created custom LLM-as-judge evaluator: %s (level=%s)", identifier, level)
    return FunctionLLMJudge(_build_prompt, name=identifier, **llm_config)


def _apply_monitor_parameter_policy(instance, eval_type: str | None) -> None:
    """Apply monitor compatibility rules without changing saved evaluator config."""
    from amp_evaluation.evaluators.base import LLMAsJudgeEvaluator

    # Arbitrary code evaluators own their parameters and SDK calls.
    if eval_type == "code" or not isinstance(instance, LLMAsJudgeEvaluator):
        return
    provider = instance.model.replace(":", "/").split("/", 1)[0].lower()
    if provider == "anthropic":
        instance._monitor_omit_temperature = True
        logger.warning(
            "%s: Temperature is not applied to Anthropic evaluations in monitors. "
            "The provider's default sampling behavior will be used; the configured value is retained.",
            instance.name,
        )


def main() -> None:
    """Main entry point for monitor job."""
    configure_logging()
    args = parse_args()

    # Read OAuth2 client credentials for publisher authentication
    idp_token_url = os.environ.get("IDP_TOKEN_URL")
    idp_client_id = os.environ.get("IDP_CLIENT_ID")
    idp_client_secret = os.environ.get("IDP_CLIENT_SECRET")
    if not all([idp_token_url, idp_client_id, idp_client_secret]):
        logger.error("IDP_TOKEN_URL, IDP_CLIENT_ID, and IDP_CLIENT_SECRET environment variables must all be set")
        sys.exit(1)
    assert idp_token_url and idp_client_id and idp_client_secret
    token_manager = OAuth2TokenManager(idp_token_url, idp_client_id, idp_client_secret)

    # LLM_API_KEY is injected from the K8s Secret (via ExternalSecret from OpenBao).
    # LLM_API_BASE is injected as a plain workflow parameter (it is a URL, not a secret).
    llm_api_key = os.environ.get("LLM_API_KEY")
    llm_api_base = os.environ.get("LLM_API_BASE")

    # The two are injected together (URL as a workflow parameter, key from the
    # Secret); exactly one being set means a broken deployment, so fail fast
    # rather than silently running judges with no gateway and no provider key.
    if bool(llm_api_key) != bool(llm_api_base):
        logger.error("LLM_API_BASE and LLM_API_KEY must be set together for gateway routing")
        sys.exit(1)

    gateway_enabled = bool(llm_api_key and llm_api_base)
    if gateway_enabled:
        assert llm_api_base and llm_api_key

        # Route every judge completion through the WSO2 gateway: the SDK sends
        # each request to this base URL and the gateway api-key is injected as a
        # request header on the underlying provider client by the evaluation lib.
        from amp_evaluation.config import get_config

        judge_cfg = get_config().llm_judge
        judge_cfg.api_base = llm_api_base
        judge_cfg.api_key = llm_api_key

        logger.debug("Configured LLM client to route through OpenAI-compatible gateway at %s", llm_api_base)

    # The score writer accepts only the run token, so without one its publish is skipped.
    writer_token: str | None = None
    if args.score_writer_endpoint:
        if not args.score_writer_endpoint.startswith("https://"):
            logger.error("--score-writer-endpoint is not an https URL; skipping the score writer publish")
        else:
            writer_token = read_run_token()
            if writer_token is None:
                logger.warning("Run token file not found; skipping the score writer publish")

    logger.info(
        "Starting monitor evaluation monitor=%s organization=%s project=%s agent=%s env=%s time_range=%s..%s sampling=%.2f",
        args.monitor_name,
        args.organization,
        args.project,
        args.agent,
        args.environment,
        args.trace_start,
        args.trace_end,
        args.sampling_rate,
    )

    # Validate time formats
    if not validate_time_format(args.trace_start):
        logger.error(
            "Invalid time format for --trace-start: %s. Expected ISO 8601 format",
            args.trace_start,
        )
        sys.exit(1)

    if not validate_time_format(args.trace_end):
        logger.error(
            "Invalid time format for --trace-end: %s. Expected ISO 8601 format",
            args.trace_end,
        )
        sys.exit(1)

    if not 0 < args.sampling_rate <= 1:
        logger.error(
            "Invalid --sampling-rate: %s. Expected a value in (0, 1]",
            args.sampling_rate,
        )
        sys.exit(1)

    # Parse evaluators JSON
    try:
        evaluators_config = json.loads(args.evaluators)
    except json.JSONDecodeError as e:
        logger.error("Invalid JSON in --evaluators: %s", e)
        sys.exit(1)

    if not evaluators_config or not isinstance(evaluators_config, list):
        logger.error("--evaluators must be a non-empty array")
        sys.exit(1)

    for i, evaluator in enumerate(evaluators_config):
        if not isinstance(evaluator, dict):
            logger.error(
                "Evaluator at index %d must be an object/dict, got %s",
                i,
                type(evaluator).__name__,
            )
            sys.exit(1)

    evaluator_names_summary = [e.get("displayName", e.get("identifier", "unknown")) for e in evaluators_config]
    logger.info("Evaluators to run: %s", ", ".join(evaluator_names_summary))
    for evaluator in evaluators_config:
        config = evaluator.get("config", {})
        if config:
            logger.debug(
                "Evaluator '%s' config: %s",
                evaluator.get("displayName", evaluator.get("identifier")),
                config,
            )

    # Create evaluator instances with configurations
    # Build identifier lookup for publish: display_name -> identifier
    display_name_to_identifier = {}
    evaluator_instances = []
    for evaluator in evaluators_config:
        identifier = evaluator.get("identifier")
        display_name = evaluator.get("displayName")
        if not identifier:
            logger.error("Evaluator missing 'identifier' field")
            sys.exit(1)
        if not display_name:
            logger.error("Evaluator missing 'displayName' field")
            sys.exit(1)

        config = evaluator.get("config", {})
        eval_type = evaluator.get("type")  # None for built-in, "code" or "llm_judge" for custom

        try:
            # Ensure model has a provider prefix for LLM-judge evaluators.
            # Known providers have the prefix stored in the DB at creation time.
            # Unknown/legacy providers fall back to openai/ with a warning.
            if eval_type == "llm_judge" and gateway_enabled and "model" in config:
                model = config["model"]
                if "/" not in model:
                    logger.warning(
                        "LLM-judge model '%s' uses an unsupported provider for monitors; "
                        "assuming OpenAI-compatible and trying to connect...",
                        model,
                    )
                    config["model"] = "openai/" + model

            if eval_type == "code":
                source = evaluator.get("source")
                if not source:
                    raise ValueError(f"Code evaluator '{identifier}' has no source")
                instance = _load_custom_code_evaluator(identifier, source, config)
            elif eval_type == "llm_judge":
                source = evaluator.get("source")
                if not source:
                    raise ValueError(f"LLM-judge evaluator '{identifier}' has no source")
                level = evaluator.get("level", "trace")
                instance = _create_custom_llm_judge(identifier, source, level, config)
            else:
                instance = builtin(identifier, **config)
            instance.name = display_name
            _apply_monitor_parameter_policy(instance, eval_type)
            evaluator_instances.append(instance)
            display_name_to_identifier[display_name] = identifier
        except Exception as e:
            logger.error("Failed to register evaluator '%s': %s", identifier, e)
            sys.exit(1)

    # Initialize and run monitor
    try:
        fetcher = TraceFetcher(
            base_url=args.traces_api_endpoint,
            organization=args.organization,
            project=args.project,
            agent=args.agent,
            environment=args.environment,
            token_provider=token_manager.get_token,
            page_size=TRACE_FETCH_PAGE_SIZE,
        )
        recorder = FetchedTraceRecorder(fetcher) if writer_token else None

        monitor = Monitor(
            evaluators=evaluator_instances,
            # Monitor only calls fetch_traces on the recorder.
            trace_fetcher=cast(TraceFetcher, recorder) if recorder else fetcher,
        )

        # Run evaluation
        result = monitor.run(
            start_time=args.trace_start,
            end_time=args.trace_end,
            sample_rate=args.sampling_rate,
        )

        # Fail if there were errors (e.g. trace fetching failed)
        if result.errors:
            for err in result.errors:
                logger.error("Evaluation error: %s", err)
            sys.exit(1)

        # Check if any traces were evaluated
        if result.traces_evaluated == 0:
            logger.warning(
                "No traces found in time range %s..%s",
                args.trace_start,
                args.trace_end,
            )
            sys.exit(0)

        # Log results
        logger.info(
            "Evaluation complete: %d evaluator(s), %d trace(s), duration=%.1fs, status=%s",
            len(evaluator_instances),
            result.traces_evaluated,
            result.duration_seconds,
            "SUCCESS" if result.success else "FAILED",
        )

        if result.scores:
            for name, summary in result.scores.items():
                passed = summary.count - (summary.skipped_count or 0)
                agg_info = ""
                agg_scores = summary.aggregated_scores
                if "mean" in agg_scores:
                    agg_info = " mean=%.3f" % agg_scores["mean"]
                elif agg_scores:
                    first_key = next(iter(agg_scores))
                    agg_info = " %s=%s" % (first_key, agg_scores[first_key])

                if summary.skipped_count:
                    logger.info(
                        "  %s: %d/%d passed (%d skipped)%s",
                        name,
                        passed,
                        summary.count,
                        summary.skipped_count,
                        agg_info,
                    )
                else:
                    logger.info("  %s: %d/%d passed%s", name, passed, summary.count, agg_info)

                # Log skip reasons with unique reason breakdown
                if summary.skipped_count:
                    skip_reason_counts: Dict[str, int] = {}
                    for score in summary.individual_scores:
                        if score.skip_reason:
                            reason = score.skip_reason
                        elif not score.is_successful and score.score is None:
                            reason = "Evaluation did not produce a score"
                        else:
                            continue
                        skip_reason_counts[reason] = skip_reason_counts.get(reason, 0) + 1

                    for reason, count in skip_reason_counts.items():
                        logger.warning("  %s: %d skip(s) — reason: %s", name, count, reason)

        # Publish scores to agent-manager
        publish_success = publish_scores(
            monitor_id=args.monitor_id,
            run_id=args.run_id,
            scores=result.scores,
            display_name_to_identifier=display_name_to_identifier,
            api_endpoint=args.publisher_endpoint,
            token_manager=token_manager,
        )

        # The score writer publish never changes the exit code.
        if writer_token and recorder:
            try:
                publish_trace_scores(
                    endpoint=args.score_writer_endpoint,
                    monitor_id=args.monitor_id,
                    run_id=args.run_id,
                    monitor_name=args.monitor_name,
                    items=writer_score_items(result.scores or {}, display_name_to_identifier, recorder.trace_ids),
                    run_token=writer_token,
                    ca_path=args.score_writer_ca,
                )
            except Exception as e:
                logger.error("Failed to publish scores to the score writer %s: %s", args.score_writer_endpoint, e)

        if not publish_success:
            logger.error("Failed to publish scores - evaluation results not persisted")
            sys.exit(1)

        # Exit with appropriate code
        sys.exit(0 if result.success else 1)

    except Exception as e:
        logger.error("Monitor execution failed: %s", e)
        logger.debug("Monitor execution failed", exc_info=True)
        sys.exit(1)


if __name__ == "__main__":
    main()
