/**
 * Copyright (c) 2026, WSO2 LLC. (https://www.wso2.com).
 *
 * WSO2 LLC. licenses this file to you under the Apache License,
 * Version 2.0 (the "License"); you may not use this file except
 * in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

import type { TraceFilters } from "@agent-management-platform/types";

export type TraceFilterKey = keyof TraceFilters;
type NumericFilterKey = "minDurationMs" | "minTokens" | "minSpanCount";
type TextFilterKey = "model" | "conversationId" | "tool" | "mcpServer" | "evaluator";
type ScoreFilterKey = "minScore" | "maxScore";

// URL param names match the API query params; this is also the chip order.
export const TRACE_FILTER_KEYS: TraceFilterKey[] = [
  "status",
  "minDurationMs",
  "minTokens",
  "minSpanCount",
  "model",
  "conversationId",
  "tool",
  "toolError",
  "mcpServer",
  "evaluator",
  "minScore",
  "maxScore",
];

const NUMERIC_KEYS: NumericFilterKey[] = ["minDurationMs", "minTokens", "minSpanCount"];
const TEXT_KEYS: TextFilterKey[] = ["model", "conversationId", "tool", "mcpServer", "evaluator"];
const SCORE_KEYS: ScoreFilterKey[] = ["minScore", "maxScore"];

// The API rejects longer text filter values.
export const MAX_TEXT_FILTER_LENGTH = 256;

export const LATENCY_PRESETS_MS = [1000, 5000, 10000, 30000];
export const TOKEN_PRESETS = [1000, 5000, 10000, 50000];
export const STEP_PRESETS = [10, 20, 50];
export const SCORE_PRESETS = [0.25, 0.5, 0.75];

/** Formats ms as seconds when whole, e.g. 5s or 1500ms. */
export const formatLatency = (ms: number) =>
  ms % 1000 === 0 ? `${ms / 1000}s` : `${ms}ms`;

/** Formats whole thousands as k, e.g. 5k. */
export const formatTokens = (n: number) =>
  n >= 1000 && n % 1000 === 0 ? `${n / 1000}k` : `${n}`;

/** Formats a 0–1 score as a percentage, e.g. 50% or 33.33%. */
export const formatScore = (score: number) => `${Number((score * 100).toFixed(2))}%`;

/** Whether a score bound is set; evaluator needs one. */
export const hasScoreBound = (filters: TraceFilters) =>
  filters.minScore !== undefined || filters.maxScore !== undefined;

// Reads filters from the URL, dropping anything the API would reject.
export function parseTraceFilters(searchParams: URLSearchParams): TraceFilters {
  const filters: TraceFilters = {};
  const status = searchParams.get("status");
  if (status === "error" || status === "ok") filters.status = status;
  for (const key of NUMERIC_KEYS) {
    const raw = searchParams.get(key) ?? "";
    // Non-negative integers only, as the API validates; parseInt would accept "5s".
    if (/^\d+$/.test(raw)) {
      const parsed = Number(raw);
      if (Number.isSafeInteger(parsed)) filters[key] = parsed;
    }
  }
  for (const key of TEXT_KEYS) {
    const raw = searchParams.get(key)?.trim();
    if (raw && raw.length <= MAX_TEXT_FILTER_LENGTH) filters[key] = raw;
  }
  if (searchParams.get("toolError") === "true") filters.toolError = true;
  for (const key of SCORE_KEYS) {
    const raw = searchParams.get(key) ?? "";
    // Plain decimals in [0, 1], as the API validates.
    if (/^(\d+\.?\d*|\.\d+)$/.test(raw) && Number(raw) <= 1) filters[key] = Number(raw);
  }
  // The API rejects minScore above maxScore; maxScore has the control, so it wins.
  if (
    filters.minScore !== undefined &&
    filters.maxScore !== undefined &&
    filters.minScore > filters.maxScore
  ) {
    delete filters.minScore;
  }
  if (filters.evaluator && !hasScoreBound(filters)) delete filters.evaluator;
  return filters;
}

// Returns a copy of searchParams with the filter params replaced by filters.
export function withTraceFilters(
  searchParams: URLSearchParams,
  filters: TraceFilters,
): URLSearchParams {
  const next = new URLSearchParams(searchParams);
  for (const key of TRACE_FILTER_KEYS) {
    // evaluator goes with the last score bound.
    const value = key === "evaluator" && !hasScoreBound(filters) ? undefined : filters[key];
    if (value === undefined || value === "" || value === false) next.delete(key);
    else next.set(key, String(value));
  }
  return next;
}

export interface TraceFilterChip {
  key: TraceFilterKey;
  label: string;
  // Other filters the chip's × clears with key.
  alsoClears?: TraceFilterKey[];
}

// One human-readable chip per set filter, in TRACE_FILTER_KEYS order.
export function traceFilterChips(filters: TraceFilters): TraceFilterChip[] {
  const chips: TraceFilterChip[] = [];
  if (filters.status) {
    chips.push({ key: "status", label: `Status: ${filters.status === "error" ? "Error" : "OK"}` });
  }
  if (filters.minDurationMs !== undefined) {
    chips.push({ key: "minDurationMs", label: `Latency ≥ ${formatLatency(filters.minDurationMs)}` });
  }
  if (filters.minTokens !== undefined) {
    chips.push({ key: "minTokens", label: `Tokens ≥ ${formatTokens(filters.minTokens)}` });
  }
  if (filters.minSpanCount !== undefined) {
    chips.push({ key: "minSpanCount", label: `Steps ≥ ${filters.minSpanCount}` });
  }
  if (filters.model) chips.push({ key: "model", label: `Model: ${filters.model}` });
  if (filters.conversationId) {
    chips.push({ key: "conversationId", label: `Conversation: ${filters.conversationId}` });
  }
  // tool with toolError means that tool failed, so they share one chip.
  if (filters.tool && filters.toolError) {
    chips.push({ key: "tool", label: `${filters.tool} failed`, alsoClears: ["toolError"] });
  } else if (filters.tool) {
    chips.push({ key: "tool", label: `Tool: ${filters.tool}` });
  } else if (filters.toolError) {
    chips.push({ key: "toolError", label: "Tool failed" });
  }
  if (filters.mcpServer) chips.push({ key: "mcpServer", label: `MCP server: ${filters.mcpServer}` });
  if (filters.evaluator) chips.push({ key: "evaluator", label: `Evaluator: ${filters.evaluator}` });
  if (filters.minScore !== undefined) {
    chips.push({ key: "minScore", label: `Score ≥ ${formatScore(filters.minScore)}` });
  }
  if (filters.maxScore !== undefined) {
    chips.push({ key: "maxScore", label: `Score ≤ ${formatScore(filters.maxScore)}` });
  }
  return chips;
}
