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

package controllers

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wso2/agent-manager/agent-manager-observer/middleware/logger"
	"github.com/wso2/agent-manager/agent-manager-observer/observer"
	"github.com/wso2/agent-manager/agent-manager-observer/opensearch"
)

const (
	// MaxSpansPerRequest is the hard cap on spans fetched per trace (used in export).
	MaxSpansPerRequest = 10000
	// maxConcurrentFetches limits concurrent GetSpanDetails calls to the Observer.
	maxConcurrentFetches = 50
	// maxConcurrentTraces limits concurrent per-trace goroutines in ExportTraces.
	maxConcurrentTraces = 10
	// maxLLMLeavesPerTrace caps the number of leaf LLM spans fetched per trace
	// when the trace-list view falls back to leaf aggregation for Input/Output
	// preview and token totals (OpenAI Agents SDK / pure-OTel agents). Realistic
	// multi-turn agents stay under this; traces beyond the cap get TokenUsage.Partial=true.
	maxLLMLeavesPerTrace = 50
	// skipLeafAggregationSpanCountThreshold short-circuits leaf aggregation
	// entirely when the trace's total span count exceeds this — a rough proxy
	// for "way more than 50 LLM leaves" that keeps the worst-case list-endpoint
	// cost bounded.
	skipLeafAggregationSpanCountThreshold = 100
)

// TracingController provides tracing functionality via the observer service.
type TracingController struct {
	observerClient observer.Client
}

// NewTracingController creates a new tracing controller.
func NewTracingController(observerClient observer.Client) *TracingController {
	return &TracingController{observerClient: observerClient}
}

// TraceQueryParams holds parameters for trace queries.
type TraceQueryParams struct {
	Organization string
	Project      *string
	Agent        *string
	Environment  *string
	StartTime    time.Time
	EndTime      time.Time
	Limit        int
	SortOrder    string
}

// SpanSummary is a lightweight span summary for the span list endpoint.
type SpanSummary struct {
	SpanID        string    `json:"spanId"`
	SpanName      string    `json:"spanName"`
	SpanKind      string    `json:"spanKind,omitempty"`
	ParentSpanID  string    `json:"parentSpanId,omitempty"`
	StartTime     time.Time `json:"startTime"`
	EndTime       time.Time `json:"endTime"`
	DurationNs    int64     `json:"durationNs"`
	Error         bool      `json:"error,omitempty"`
	StatusMessage string    `json:"statusMessage,omitempty"`
}

// returns (e.g. "Error"), which is enough for the trace tree to flag failed spans.
func spanStatusIsError(s *observer.SpanStatus) bool {
	return s != nil && strings.EqualFold(s.Code, "error")
}

// SpanListResponse is the response for GET /api/v1/traces/{traceId}/spans.
type SpanListResponse struct {
	Spans      []SpanSummary `json:"spans"`
	TotalCount int           `json:"totalCount"`
}

// GetTraceOverviews fetches a page of traces with root-span enrichment (input, output, tokenUsage).
// It calls QueryTraces once, then fetches root span details in parallel (one per trace in the page).
func (c *TracingController) GetTraceOverviews(ctx context.Context, params TraceQueryParams) (*opensearch.TraceOverviewResponse, error) {
	log := logger.GetLogger(ctx)

	sortOrder := params.SortOrder
	req := observer.TracesQueryRequest{
		StartTime: params.StartTime,
		EndTime:   params.EndTime,
		Limit:     &params.Limit,
		SortOrder: &sortOrder,
		SearchScope: observer.ComponentSearchScope{
			Namespace:   c.observerClient.NamespaceFor(params.Organization),
			Project:     params.Project,
			Component:   params.Agent,
			Environment: params.Environment,
		},
	}

	tracesResp, err := c.observerClient.QueryTraces(ctx, req)
	if err != nil {
		return nil, err
	}

	if len(tracesResp.Traces) == 0 {
		return &opensearch.TraceOverviewResponse{
			Traces:     []opensearch.TraceOverview{},
			TotalCount: tracesResp.Total,
		}, nil
	}

	// Fetch root spans and run per-trace enrichment in parallel.
	// outerSem caps how many traces are being enriched at once; innerSem caps
	// the total observer round-trips across all enrichments in flight (root
	// fetches plus anything enrichTraceOverview fetches inside).
	// Two pools avoid the deadlock a single shared pool would hit when every
	// outer slot is held by a goroutine waiting on a fetch slot.
	type result struct {
		idx        int
		span       *opensearch.Span
		input      interface{}
		output     interface{}
		tokenUsage *opensearch.TokenUsage
		err        error
	}
	results := make([]result, len(tracesResp.Traces))
	outerSem := make(chan struct{}, maxConcurrentTraces)
	innerSem := make(chan struct{}, maxConcurrentFetches)
	var wg sync.WaitGroup

	for i, t := range tracesResp.Traces {
		if t.RootSpanID == "" {
			log.Warn("trace has no rootSpanId, skipping", "traceId", t.TraceID)
			continue
		}
		wg.Add(1)
		go func(idx int, t observer.TraceInfo) {
			defer wg.Done()
			outerSem <- struct{}{}
			defer func() { <-outerSem }()

			innerSem <- struct{}{}
			details, err := c.observerClient.GetSpanDetails(ctx, t.TraceID, t.RootSpanID)
			<-innerSem
			if err != nil {
				results[idx] = result{idx: idx, err: err}
				return
			}
			enriched := opensearch.ProcessSpan(observer.ConvertSpanDetailsToSpan(t.TraceID, details))
			input, output, tokens := c.enrichTraceOverview(ctx, params, t, &enriched, innerSem)
			results[idx] = result{idx: idx, span: &enriched, input: input, output: output, tokenUsage: tokens}
		}(i, t)
	}
	wg.Wait()

	overviews := make([]opensearch.TraceOverview, 0, len(tracesResp.Traces))
	for i, t := range tracesResp.Traces {
		res := results[i]
		if res.err != nil {
			log.Warn("failed to fetch root span details, skipping trace",
				"traceId", t.TraceID, "err", res.err)
			continue
		}
		if res.span == nil {
			continue
		}
		rootSpan := res.span
		traceStatus := opensearch.ExtractTraceStatus([]opensearch.Span{*rootSpan})

		overviews = append(overviews, opensearch.TraceOverview{
			TraceID:         t.TraceID,
			RootSpanID:      t.RootSpanID,
			RootSpanName:    t.RootSpanName,
			RootSpanKind:    string(opensearch.DetermineSpanType(*rootSpan)),
			StartTime:       t.StartTime.Format(time.RFC3339Nano),
			EndTime:         t.EndTime.Format(time.RFC3339Nano),
			DurationInNanos: t.DurationNs,
			SpanCount:       t.SpanCount,
			TokenUsage:      res.tokenUsage,
			Status:          traceStatus,
			Input:           res.input,
			Output:          res.output,
		})
	}

	log.Info("Retrieved trace overviews",
		"totalCount", len(overviews),
		"returned", len(overviews))

	return &opensearch.TraceOverviewResponse{
		Traces:     overviews,
		TotalCount: tracesResp.Total,
	}, nil
}

// enrichTraceOverview computes Input/Output/Tokens for one row of the trace
// list, cascading through three sources in order of cost:
//
//  1. The root span's own attributes (older Traceloop entity.input/output
//     and CrewAI roll-up). Free — root span is already fetched.
//  2. The immediate child of the root, typically a chain span like
//     LangGraph.workflow that Traceloop's LangChain instrumentation still
//     decorates with traceloop.entity.input/output. Costs +1 GetSpanDetails.
//  3. Leaf LLM spans (anything ending in ".chat"). Used for OpenAI Agents
//     SDK / pure-OTel agents where neither the root nor any chain span
//     carries the conversation. Bounded: skipped entirely when the trace's
//     total span count exceeds skipLeafAggregationSpanCountThreshold; up to
//     maxLLMLeavesPerTrace leaves are fetched in parallel; TokenUsage.Partial
//     is set true when the cap truncates the aggregation.
//
// Each step only fills in fields the earlier step left nil — so a CrewAI
// trace that gets all three from step 1 incurs no extra calls.
//
// Token usage from traceloop.entity.output is used only when step 3 finds no
// leaf LLM spans to sum.
//
// fetchSem bounds the total observer fetches across all concurrent
// enrichments — callers pass a shared semaphore so a 50-trace page can't
// fan out into thousands of in-flight requests.
func (c *TracingController) enrichTraceOverview(
	ctx context.Context,
	params TraceQueryParams,
	traceInfo observer.TraceInfo,
	rootSpan *opensearch.Span,
	fetchSem chan struct{},
) (input interface{}, output interface{}, tokenUsage *opensearch.TokenUsage) {
	// Step 1: root span attributes.
	if opensearch.IsCrewAISpan(rootSpan.Attributes) {
		input, output = opensearch.ExtractCrewAIRootSpanInputOutput(rootSpan)
		tokenUsage = opensearch.ExtractCrewAITraceTokenUsage(rootSpan)
	} else {
		input, output = opensearch.ExtractRootSpanInputOutput(rootSpan)
	}
	if tokenUsage == nil {
		tokenUsage = opensearch.ExtractTokenUsage([]opensearch.Span{*rootSpan})
	}
	entityTokens := opensearch.ExtractTokenUsageFromEntityOutput(rootSpan)

	// If the root covered everything, short-circuit — no extra fetches.
	if input != nil && output != nil && tokenUsage != nil {
		return input, output, tokenUsage
	}

	// Both steps 2 and 3 need the per-trace span list. Fetch it once.
	spans, ok := c.fetchTraceSpanSummaries(ctx, params, traceInfo, fetchSem)
	if !ok {
		return input, output, firstTokenUsage(tokenUsage, entityTokens)
	}

	// Step 2: immediate child of the root (Traceloop chain span path).
	if input == nil || output == nil || tokenUsage == nil {
		if childInput, childOutput, childTokens, childEntityTokens, ok := c.tryChildChainSpan(ctx, traceInfo.TraceID, rootSpan.SpanID, spans, fetchSem); ok {
			if input == nil {
				input = childInput
			}
			if output == nil {
				output = childOutput
			}
			if tokenUsage == nil {
				tokenUsage = childTokens
			}
			if entityTokens == nil {
				entityTokens = childEntityTokens
			}
		}
	}

	// Step 3: leaf LLM aggregation (OpenAI Agents SDK / pure-OTel path).
	if input == nil || output == nil || tokenUsage == nil {
		if traceInfo.SpanCount > skipLeafAggregationSpanCountThreshold {
			logger.GetLogger(ctx).Debug("skipping leaf-LLM aggregation: trace exceeds spanCount threshold",
				"traceId", traceInfo.TraceID,
				"spanCount", traceInfo.SpanCount,
				"threshold", skipLeafAggregationSpanCountThreshold)
		} else {
			leafInput, leafOutput, leafTokens := c.aggregateFromLeafLLMSpans(ctx, traceInfo.TraceID, spans, fetchSem)
			if input == nil {
				input = leafInput
			}
			if output == nil {
				output = leafOutput
			}
			if tokenUsage == nil {
				tokenUsage = leafTokens
			}
		}
	}

	return input, output, firstTokenUsage(tokenUsage, entityTokens)
}

// firstTokenUsage returns the first non-nil usage, so a trusted aggregate wins
// over the entity.output fallback.
func firstTokenUsage(usages ...*opensearch.TokenUsage) *opensearch.TokenUsage {
	for _, u := range usages {
		if u != nil {
			return u
		}
	}
	return nil
}

// fetchTraceSpanSummaries calls QueryTraceSpans for one trace and returns
// the span-summary list, mirroring the request shape used elsewhere in this
// controller. Acquires a slot on fetchSem for the duration of the call so
// the call counts against the shared cross-trace fetch budget. Returns
// ok=false on error (logged as a warning); callers fall back gracefully to
// whatever they already extracted.
func (c *TracingController) fetchTraceSpanSummaries(
	ctx context.Context,
	params TraceQueryParams,
	traceInfo observer.TraceInfo,
	fetchSem chan struct{},
) ([]observer.SpanInfo, bool) {
	log := logger.GetLogger(ctx)

	spanLimit := traceInfo.SpanCount
	if spanLimit <= 0 || spanLimit > MaxSpansPerRequest {
		spanLimit = MaxSpansPerRequest
	}
	fetchSem <- struct{}{}
	spansResp, err := c.observerClient.QueryTraceSpans(ctx, traceInfo.TraceID, observer.TracesQueryRequest{
		StartTime: params.StartTime,
		EndTime:   params.EndTime,
		Limit:     &spanLimit,
		SearchScope: observer.ComponentSearchScope{
			Namespace:   c.observerClient.NamespaceFor(params.Organization),
			Project:     params.Project,
			Component:   params.Agent,
			Environment: params.Environment,
		},
	})
	<-fetchSem
	if err != nil {
		log.Warn("enrichTraceOverview: QueryTraceSpans failed, skipping enrichment",
			"traceId", traceInfo.TraceID, "err", err)
		return nil, false
	}
	return spansResp.Spans, true
}

// tryChildChainSpan fetches the earliest immediate child of the root span
// and runs the same entity.input/output / entity.output token extractors
// against it. Covers LangChain / LangGraph agents where Traceloop emits the
// conversation summary on a chain span (e.g. LangGraph.workflow) right under
// an attribute-empty `invoke_agent` root.
//
// tokens comes from the span's gen_ai.usage.* attributes; entityTokens from
// its traceloop.entity.output.
//
// Returns ok=false when no immediate child is found or the fetch fails.
func (c *TracingController) tryChildChainSpan(
	ctx context.Context,
	traceID string,
	rootSpanID string,
	spans []observer.SpanInfo,
	fetchSem chan struct{},
) (input interface{}, output interface{}, tokens, entityTokens *opensearch.TokenUsage, ok bool) {
	log := logger.GetLogger(ctx)

	// Pick the earliest-started direct child of the root, skipping leaf LLM
	// spans. We're looking for a chain/agent wrapper (e.g. LangGraph.workflow)
	// that summarizes the whole conversation, not a single LLM call — those
	// belong to step 3's full-trace aggregation, not a per-span lookup.
	var childID string
	var childStart time.Time
	for _, s := range spans {
		if s.ParentSpanID != rootSpanID {
			continue
		}
		if opensearch.IsLLMLeafSpan(s.SpanName) {
			continue
		}
		if childID == "" || s.StartTime.Before(childStart) {
			childID = s.SpanID
			childStart = s.StartTime
		}
	}
	if childID == "" {
		return nil, nil, nil, nil, false
	}

	fetchSem <- struct{}{}
	details, err := c.observerClient.GetSpanDetails(ctx, traceID, childID)
	<-fetchSem
	if err != nil {
		log.Warn("tryChildChainSpan: GetSpanDetails failed",
			"traceId", traceID, "childSpanId", childID, "err", err)
		return nil, nil, nil, nil, false
	}
	childSpan := opensearch.ProcessSpan(observer.ConvertSpanDetailsToSpan(traceID, details))

	input, output = opensearch.ExtractRootSpanInputOutput(&childSpan)
	tokens = opensearch.ExtractTokenUsage([]opensearch.Span{childSpan})
	entityTokens = opensearch.ExtractTokenUsageFromEntityOutput(&childSpan)
	return input, output, tokens, entityTokens, true
}

// aggregateFromLeafLLMSpans fetches up to maxLLMLeavesPerTrace leaf LLM spans
// (name matches IsLLMLeafSpan) and aggregates token usage across them plus
// first/last message previews. Used when neither the root nor a child chain
// span carries the data (OpenAI Agents SDK / pure-OTel agents).
//
// Fetches share fetchSem with every other enrichment in flight, so leaf fan-
// out across many concurrent traces doesn't flood the upstream observer.
//
// If the trace has more LLM leaves than the cap, the returned TokenUsage has
// Partial=true so the UI can render an "approximate" marker.
func (c *TracingController) aggregateFromLeafLLMSpans(
	ctx context.Context,
	traceID string,
	spans []observer.SpanInfo,
	fetchSem chan struct{},
) (input interface{}, output interface{}, tokens *opensearch.TokenUsage) {
	log := logger.GetLogger(ctx)

	// Filter to leaf LLM spans, ordered by start time.
	leaves := make([]observer.SpanInfo, 0)
	for _, s := range spans {
		if opensearch.IsLLMLeafSpan(s.SpanName) {
			leaves = append(leaves, s)
		}
	}
	if len(leaves) == 0 {
		return nil, nil, nil
	}
	sort.Slice(leaves, func(i, j int) bool { return leaves[i].StartTime.Before(leaves[j].StartTime) })

	totalLeaves := len(leaves)
	partial := false
	if totalLeaves > maxLLMLeavesPerTrace {
		leaves = leaves[:maxLLMLeavesPerTrace]
		partial = true
		log.Debug("aggregateFromLeafLLMSpans: capping leaf fetches",
			"traceId", traceID, "totalLeaves", totalLeaves, "cap", maxLLMLeavesPerTrace)
	}

	fetched := make([]opensearch.Span, len(leaves))
	var wg sync.WaitGroup
	for i, leaf := range leaves {
		wg.Add(1)
		go func(idx int, spanID string) {
			defer wg.Done()
			fetchSem <- struct{}{}
			defer func() { <-fetchSem }()

			details, err := c.observerClient.GetSpanDetails(ctx, traceID, spanID)
			if err != nil {
				log.Warn("aggregateFromLeafLLMSpans: GetSpanDetails failed for leaf",
					"traceId", traceID, "spanId", spanID, "err", err)
				return
			}
			fetched[idx] = opensearch.ProcessSpan(observer.ConvertSpanDetailsToSpan(traceID, details))
		}(i, leaf.SpanID)
	}
	wg.Wait()

	// Trim out any leaves that failed to fetch (zero-value Span).
	validLeaves := make([]opensearch.Span, 0, len(fetched))
	for _, s := range fetched {
		if s.SpanID != "" {
			validLeaves = append(validLeaves, s)
		}
	}
	if len(validLeaves) == 0 {
		return nil, nil, nil
	}

	tokens = opensearch.ExtractTokenUsage(validLeaves)
	if tokens != nil && partial {
		tokens.Partial = true
	}

	// Input preview from the first leaf that actually carries messages. A
	// framework that wraps the provider call in its own LLM span (Strands opens
	// "chat" around "openai.chat") puts a content-free span first, and reading
	// only validLeaves[0] leaves the trace list's Input column blank.
	for i := range validLeaves {
		if v := opensearch.ExtractInputPreviewFromLeaf(&validLeaves[i]); v != nil {
			input = v
			break
		}
	}
	// Output stays pinned to the last leaf: it is the turn's final model call,
	// and walking backwards would surface an earlier turn's answer instead.
	output = opensearch.ExtractOutputPreviewFromLeaf(&validLeaves[len(validLeaves)-1])
	return input, output, tokens
}

// GetTraceSpans fetches span summaries for a specific trace (no attributes).
func (c *TracingController) GetTraceSpans(ctx context.Context, traceID string, params TraceQueryParams) (*SpanListResponse, error) {
	log := logger.GetLogger(ctx)

	sortOrder := params.SortOrder
	req := observer.TracesQueryRequest{
		StartTime: params.StartTime,
		EndTime:   params.EndTime,
		Limit:     &params.Limit,
		SortOrder: &sortOrder,
		SearchScope: observer.ComponentSearchScope{
			Namespace:   c.observerClient.NamespaceFor(params.Organization),
			Project:     params.Project,
			Component:   params.Agent,
			Environment: params.Environment,
		},
	}

	spansResp, err := c.observerClient.QueryTraceSpans(ctx, traceID, req)
	if err != nil {
		return nil, err
	}

	summaries := make([]SpanSummary, 0, len(spansResp.Spans))
	for _, s := range spansResp.Spans {
		summary := SpanSummary{
			SpanID:       s.SpanID,
			SpanName:     s.SpanName,
			SpanKind:     string(opensearch.DetermineSpanKindFromName(s.SpanName)),
			ParentSpanID: s.ParentSpanID,
			StartTime:    s.StartTime,
			EndTime:      s.EndTime,
			DurationNs:   s.DurationNs,
			Error:        spanStatusIsError(s.Status),
		}
		if s.Status != nil {
			summary.StatusMessage = s.Status.Message
		}
		summaries = append(summaries, summary)
	}

	log.Info("Retrieved trace spans",
		"traceId", traceID,
		"totalCount", spansResp.Total,
		"returned", len(summaries))

	return &SpanListResponse{
		Spans:      summaries,
		TotalCount: spansResp.Total,
	}, nil
}

// GetSpanDetail fetches full span details including enriched AmpAttributes.
func (c *TracingController) GetSpanDetail(ctx context.Context, traceID, spanID string) (*opensearch.Span, error) {
	details, err := c.observerClient.GetSpanDetails(ctx, traceID, spanID)
	if err != nil {
		return nil, err
	}

	span := observer.ConvertSpanDetailsToSpan(traceID, details)
	enriched := opensearch.ProcessSpan(span)
	return &enriched, nil
}

// ExportTraces fetches complete traces with all spans fully enriched for export.
// Observer calls: 1 QueryTraces + N QueryTraceSpans (spans carry attributes
// inline via includeAttributes). Concurrency is bounded by maxConcurrentTraces
// outer goroutines. Any single failure aborts the entire export.
func (c *TracingController) ExportTraces(ctx context.Context, params TraceQueryParams) (*opensearch.TraceExportResponse, error) {
	log := logger.GetLogger(ctx)

	sortOrder := params.SortOrder
	req := observer.TracesQueryRequest{
		StartTime: params.StartTime,
		EndTime:   params.EndTime,
		Limit:     &params.Limit,
		SortOrder: &sortOrder,
		SearchScope: observer.ComponentSearchScope{
			Namespace:   c.observerClient.NamespaceFor(params.Organization),
			Project:     params.Project,
			Component:   params.Agent,
			Environment: params.Environment,
		},
	}

	tracesResp, err := c.observerClient.QueryTraces(ctx, req)
	if err != nil {
		return nil, err
	}

	if len(tracesResp.Traces) == 0 {
		return &opensearch.TraceExportResponse{
			Traces:     []opensearch.FullTrace{},
			TotalCount: tracesResp.Total,
		}, nil
	}

	type traceResult struct {
		idx       int
		fullTrace *opensearch.FullTrace
	}

	results := make([]traceResult, len(tracesResp.Traces))
	var truncated atomic.Bool

	// Fail-fast: first error cancels all in-flight requests.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var firstErr error
	var errOnce sync.Once

	outerSem := make(chan struct{}, maxConcurrentTraces)
	var wg sync.WaitGroup

	for i, t := range tracesResp.Traces {
		wg.Add(1)
		go func(idx int, traceInfo observer.TraceInfo) {
			defer wg.Done()

			outerSem <- struct{}{}
			defer func() { <-outerSem }()

			if ctx.Err() != nil {
				return
			}

			spanLimit := traceInfo.SpanCount
			if spanLimit <= 0 || spanLimit > MaxSpansPerRequest {
				spanLimit = MaxSpansPerRequest
			}

			spansResp, err := c.observerClient.QueryTraceSpans(ctx, traceInfo.TraceID, observer.TracesQueryRequest{
				StartTime:         params.StartTime,
				EndTime:           params.EndTime,
				Limit:             &spanLimit,
				IncludeAttributes: true,
				SearchScope: observer.ComponentSearchScope{
					Namespace:   c.observerClient.NamespaceFor(params.Organization),
					Project:     params.Project,
					Component:   params.Agent,
					Environment: params.Environment,
				},
			})
			if err != nil {
				errOnce.Do(func() {
					firstErr = fmt.Errorf("trace %s: query spans: %w", traceInfo.TraceID, err)
					cancel()
				})
				return
			}

			if traceInfo.SpanCount > MaxSpansPerRequest {
				truncated.Store(true)
			}

			// Spans already carry attributes/kind/status from the bulk
			// QueryTraceSpans call (includeAttributes=true), so convert them
			// directly — no per-span GetSpanDetails round-trips.
			spans := make([]opensearch.Span, 0, len(spansResp.Spans))
			for _, s := range spansResp.Spans {
				enriched := opensearch.ProcessSpan(observer.ConvertSpanInfoToSpan(traceInfo.TraceID, s))
				spans = append(spans, enriched)
			}
			sort.Slice(spans, func(i, j int) bool {
				return spans[i].StartTime.Before(spans[j].StartTime)
			})

			// Find root span by the RootSpanID the Observer already identified.
			// Fallback: also accept a span with an empty or all-zero parentSpanId,
			// since some OTEL exporters use "0000000000000000" instead of "".
			var rootSpan *opensearch.Span
			for k := range spans {
				if spans[k].SpanID == traceInfo.RootSpanID {
					rootSpan = &spans[k]
					break
				}
			}
			if rootSpan == nil {
				// Fallback for traces where the Observer RootSpanID is absent.
				for k := range spans {
					p := spans[k].ParentSpanID
					if p == "" || p == "0000000000000000" {
						rootSpan = &spans[k]
						break
					}
				}
			}
			if rootSpan == nil {
				errOnce.Do(func() {
					firstErr = fmt.Errorf("trace %s: no root span found", traceInfo.TraceID)
					cancel()
				})
				return
			}

			// Extract input/output. Agent-rooted traces (e.g. a LangGraph
			// invoke_agent wrapper) carry no traceloop.entity.output on the
			// root, so fall back through the child chain span and leaf-LLM
			// spans — otherwise the exported trace has a blank response and the
			// LLM judge scores nothing. All spans are already in memory here.
			input, output := opensearch.ExtractTraceInputOutputWithFallback(rootSpan, spans)

			tokenUsage := opensearch.ExtractTokenUsage(spans)
			traceStatus := opensearch.ExtractTraceStatus(spans)

			// Extract taskId / trialId from root span baggage attributes.
			var taskID, trialID string
			if rootSpan.Attributes != nil {
				if v, ok := rootSpan.Attributes["task.id"].(string); ok {
					taskID = v
				}
				if v, ok := rootSpan.Attributes["trial.id"].(string); ok {
					trialID = v
				}
			}

			results[idx] = traceResult{
				idx: idx,
				fullTrace: &opensearch.FullTrace{
					TraceID:         traceInfo.TraceID,
					RootSpanID:      rootSpan.SpanID,
					RootSpanName:    rootSpan.Name,
					RootSpanKind:    string(opensearch.DetermineSpanType(*rootSpan)),
					StartTime:       traceInfo.StartTime.Format(time.RFC3339Nano),
					EndTime:         traceInfo.EndTime.Format(time.RFC3339Nano),
					DurationInNanos: traceInfo.DurationNs,
					SpanCount:       traceInfo.SpanCount,
					TokenUsage:      tokenUsage,
					Status:          traceStatus,
					Input:           input,
					Output:          output,
					TaskId:          taskID,
					TrialId:         trialID,
					Spans:           spans,
				},
			}
		}(i, t)
	}
	wg.Wait()

	if firstErr != nil {
		return nil, firstErr
	}

	fullTraces := make([]opensearch.FullTrace, 0, len(results))
	for _, r := range results {
		if r.fullTrace != nil {
			fullTraces = append(fullTraces, *r.fullTrace)
		}
	}

	log.Info("Completed trace export",
		"totalCount", tracesResp.Total,
		"exported", len(fullTraces),
		"truncated", truncated.Load())

	return &opensearch.TraceExportResponse{
		Traces:     fullTraces,
		TotalCount: tracesResp.Total,
		Truncated:  truncated.Load(),
	}, nil
}
