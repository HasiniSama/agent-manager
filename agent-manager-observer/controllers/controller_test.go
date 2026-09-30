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
	"sync/atomic"
	"testing"
	"time"

	"github.com/wso2/agent-manager/agent-manager-observer/observer"
	"github.com/wso2/agent-manager/agent-manager-observer/opensearch"
)

// fakeObserverClient is a minimal observer.Client implementation that the
// cascade tests use to script trace-overview enrichment scenarios. Each test
// installs the spans for a single trace; getSpanDetailsCalls counts how many
// GetSpanDetails calls the cascade made so we can verify the cost guards.
type fakeObserverClient struct {
	// rootSpan is returned by GetSpanDetails when called for the root span.
	rootSpan *observer.SpanDetailsResponse
	// traces is the list returned by QueryTraces (export path).
	traces []observer.TraceInfo
	// spans is the list returned by QueryTraceSpans.
	spans []observer.SpanInfo
	// spanDetails maps spanID → detail response for GetSpanDetails lookups
	// (excluding root, which is rootSpan).
	spanDetails map[string]*observer.SpanDetailsResponse

	// lastSpansReq records the request passed to the most recent
	// QueryTraceSpans call so export tests can assert IncludeAttributes.
	lastSpansReq observer.TracesQueryRequest

	getSpanDetailsCalls  int32
	queryTraceSpansCalls int32

	// defaultNamespace is returned by NamespaceFor, mirroring the real client.
	defaultNamespace string
}

func (f *fakeObserverClient) QueryTraces(_ context.Context, _ observer.TracesQueryRequest) (*observer.TracesQueryResponse, error) {
	return &observer.TracesQueryResponse{Traces: f.traces, Total: len(f.traces)}, nil
}

func (f *fakeObserverClient) QueryTraceSpans(_ context.Context, _ string, req observer.TracesQueryRequest) (*observer.TraceSpansQueryResponse, error) {
	atomic.AddInt32(&f.queryTraceSpansCalls, 1)
	f.lastSpansReq = req
	return &observer.TraceSpansQueryResponse{Spans: f.spans, Total: len(f.spans)}, nil
}

func (f *fakeObserverClient) NamespaceFor(_ string) string {
	return f.defaultNamespace
}

func (f *fakeObserverClient) QueryLogs(_ context.Context, _ observer.LogsQueryRequest) (*observer.LogsQueryResponse, error) {
	return &observer.LogsQueryResponse{}, nil
}

func (f *fakeObserverClient) QueryMetrics(_ context.Context, _ observer.MetricsQueryRequest) (*observer.ResourceMetricsTimeSeries, error) {
	return &observer.ResourceMetricsTimeSeries{}, nil
}

func (f *fakeObserverClient) GetSpanDetails(_ context.Context, _, spanID string) (*observer.SpanDetailsResponse, error) {
	atomic.AddInt32(&f.getSpanDetailsCalls, 1)
	if f.rootSpan != nil && spanID == f.rootSpan.SpanID {
		return f.rootSpan, nil
	}
	if d, ok := f.spanDetails[spanID]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("fake: span %s not found", spanID)
}

// makeRootSpan constructs an opensearch.Span suitable for passing to
// enrichTraceOverview as the rootSpan argument.
func makeRootSpan(spanID string, attrs map[string]interface{}) *opensearch.Span {
	return &opensearch.Span{
		SpanID:     spanID,
		Name:       "invoke_agent LangGraph",
		Attributes: attrs,
	}
}

// baseTraceInfo returns a TraceInfo with sane defaults; tests override
// SpanCount where they need to exercise the cost guard.
func baseTraceInfo(spanCount int) observer.TraceInfo {
	return observer.TraceInfo{
		TraceID:    "trace-1",
		RootSpanID: "root",
		StartTime:  time.Now().Add(-1 * time.Hour),
		EndTime:    time.Now(),
		SpanCount:  spanCount,
	}
}

func baseParams() TraceQueryParams {
	project := "proj"
	component := "comp"
	environment := "env"
	return TraceQueryParams{
		Organization: "ns",
		Project:      &project,
		Agent:        &component,
		Environment:  &environment,
		StartTime:    time.Now().Add(-1 * time.Hour),
		EndTime:      time.Now(),
		Limit:        50,
		SortOrder:    "desc",
	}
}

// testFetchSem returns a fresh fetch-budget semaphore sized for tests. The
// cascade's per-trace fetches share this channel in production; tests give
// each call its own channel for isolation.
func testFetchSem() chan struct{} { return make(chan struct{}, maxConcurrentFetches) }

// (a) Root span carries entity.input/output and its own gen_ai.usage.*
// report → the cascade short-circuits at step 1 with no extra fetches.
func TestEnrichTraceOverview_RootHasEntityAndUsageShortCircuits(t *testing.T) {
	root := makeRootSpan("root", map[string]interface{}{
		"traceloop.entity.input":     `{"inputs":"hello there"}`,
		"traceloop.entity.output":    `{"outputs":{"messages":[{"kwargs":{"content":"hi back"}}]}}`,
		"gen_ai.usage.input_tokens":  float64(10),
		"gen_ai.usage.output_tokens": float64(3),
	})
	fake := &fakeObserverClient{rootSpan: &observer.SpanDetailsResponse{SpanID: "root"}}
	c := NewTracingController(fake)

	input, output, tokens := c.enrichTraceOverview(context.Background(), baseParams(), baseTraceInfo(5), root, testFetchSem())

	if input == nil || output == nil {
		t.Errorf("expected input/output from root, got input=%v output=%v", input, output)
	}
	if tokens == nil || tokens.TotalTokens != 13 {
		t.Errorf("expected tokens from root, got %+v", tokens)
	}
	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != 0 {
		t.Errorf("expected no extra GetSpanDetails calls, got %d", got)
	}
	if got := atomic.LoadInt32(&fake.queryTraceSpansCalls); got != 0 {
		t.Errorf("expected no QueryTraceSpans calls, got %d", got)
	}
}

// (a2) Root entity.output carries token_usage but the trace has no leaf LLM
// spans → the entity.output usage is used as the fallback.
func TestEnrichTraceOverview_RootEntityTokensUsedWhenNoLeaves(t *testing.T) {
	root := makeRootSpan("root", map[string]interface{}{
		"traceloop.entity.input":  `{"inputs":"hello there"}`,
		"traceloop.entity.output": `{"outputs":{"messages":[{"kwargs":{"content":"hi back","response_metadata":{"token_usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}}}]}}`,
	})
	fake := &fakeObserverClient{rootSpan: &observer.SpanDetailsResponse{SpanID: "root"}}
	c := NewTracingController(fake)

	_, _, tokens := c.enrichTraceOverview(context.Background(), baseParams(), baseTraceInfo(5), root, testFetchSem())

	if tokens == nil || tokens.TotalTokens != 13 {
		t.Errorf("expected entity.output fallback tokens, got %+v", tokens)
	}
	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != 0 {
		t.Errorf("expected no GetSpanDetails calls (no child, no leaves), got %d", got)
	}
}

// (b) Root span empty, but the immediate child chain span carries
// traceloop.entity.input/output (LangGraph pattern). Step 2 must fill it in.
func TestEnrichTraceOverview_FallsBackToChildChainSpan(t *testing.T) {
	root := makeRootSpan("root", map[string]interface{}{})
	// Two children of root: a non-chain span starting later, and the chain
	// span starting first. tryChildChainSpan picks the earliest.
	startEarly := time.Now().Add(-10 * time.Minute)
	startLate := time.Now().Add(-5 * time.Minute)
	fake := &fakeObserverClient{
		spans: []observer.SpanInfo{
			{SpanID: "chain-1", SpanName: "LangGraph.workflow", ParentSpanID: "root", StartTime: startEarly},
			{SpanID: "other", SpanName: "execute_task something", ParentSpanID: "root", StartTime: startLate},
		},
		spanDetails: map[string]*observer.SpanDetailsResponse{
			"chain-1": {
				SpanID: "chain-1", SpanName: "LangGraph.workflow",
				Attributes: map[string]interface{}{
					"traceloop.entity.input":  `{"inputs":"chain in"}`,
					"traceloop.entity.output": `{"outputs":{"messages":[{"kwargs":{"content":"chain out","response_metadata":{"token_usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}}}]}}`,
				},
			},
		},
	}
	c := NewTracingController(fake)

	input, output, tokens := c.enrichTraceOverview(context.Background(), baseParams(), baseTraceInfo(5), root, testFetchSem())

	if input == nil || output == nil {
		t.Errorf("expected input/output from chain child, got input=%v output=%v", input, output)
	}
	if tokens == nil || tokens.TotalTokens != 25 {
		t.Errorf("expected tokens from chain child, got %+v", tokens)
	}
	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != 1 {
		t.Errorf("expected exactly 1 GetSpanDetails (child), got %d", got)
	}
}

// (c) Root and child empty, but leaf LLM spans carry gen_ai.input.messages /
// gen_ai.output.messages / gen_ai.usage.* — the OpenAI Agents SDK case. Step
// 3 aggregates: token sum across all leaves, first-leaf input, last-leaf output.
func TestEnrichTraceOverview_AggregatesFromLeafLLMSpans(t *testing.T) {
	root := makeRootSpan("root", map[string]interface{}{})
	start := time.Now().Add(-10 * time.Minute)
	fake := &fakeObserverClient{
		spans: []observer.SpanInfo{
			{SpanID: "leaf-1", SpanName: "openai.chat", ParentSpanID: "root", StartTime: start},
			{SpanID: "leaf-2", SpanName: "openai.chat", ParentSpanID: "root", StartTime: start.Add(1 * time.Minute)},
		},
		spanDetails: map[string]*observer.SpanDetailsResponse{
			"leaf-1": {
				SpanID: "leaf-1", SpanName: "openai.chat",
				Attributes: map[string]interface{}{
					"gen_ai.input.messages":      `[{"role":"user","parts":[{"type":"text","content":"first user msg"}]}]`,
					"gen_ai.output.messages":     `[{"role":"assistant","parts":[{"type":"text","content":"first assistant"}]}]`,
					"gen_ai.usage.input_tokens":  float64(100),
					"gen_ai.usage.output_tokens": float64(20),
				},
			},
			"leaf-2": {
				SpanID: "leaf-2", SpanName: "openai.chat",
				Attributes: map[string]interface{}{
					"gen_ai.input.messages":      `[{"role":"user","parts":[{"type":"text","content":"second user msg"}]}]`,
					"gen_ai.output.messages":     `[{"role":"assistant","parts":[{"type":"text","content":"final answer"}]}]`,
					"gen_ai.usage.input_tokens":  float64(50),
					"gen_ai.usage.output_tokens": float64(10),
				},
			},
		},
	}
	c := NewTracingController(fake)

	input, output, tokens := c.enrichTraceOverview(context.Background(), baseParams(), baseTraceInfo(5), root, testFetchSem())

	if input != "first user msg" {
		t.Errorf("input = %v, want first user msg", input)
	}
	if output != "final answer" {
		t.Errorf("output = %v, want last assistant msg", output)
	}
	if tokens == nil || tokens.InputTokens != 150 || tokens.OutputTokens != 30 || tokens.TotalTokens != 180 {
		t.Errorf("tokens = %+v, want sum across leaves (150/30/180)", tokens)
	}
	if tokens.Partial {
		t.Errorf("expected Partial=false (under cap), got true")
	}
}

// LangGraph trace with three LLM calls: tokens are the sum of the leaf LLM
// spans, not the usage in LangGraph.workflow's entity.output.
func TestEnrichTraceOverview_LangGraphSumsLeavesOverEntityOutput(t *testing.T) {
	root := makeRootSpan("root", map[string]interface{}{})
	start := time.Now().Add(-10 * time.Minute)
	aiMsg := func(in, out int) string {
		return fmt.Sprintf(`{"kwargs":{"type":"ai","content":"a","usage_metadata":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}}`, in, out, in+out)
	}
	workflowOutput := `{"outputs":{"messages":[` +
		`{"kwargs":{"type":"human","content":"earlier turn"}},` + aiMsg(2933, 26) + `,` +
		`{"kwargs":{"type":"human","content":"reset my password"}},` +
		aiMsg(2982, 31) + `,` + aiMsg(3086, 92) + `,` + aiMsg(3285, 63) + `]}}`

	leafUsage := [][2]int{{2982, 31}, {3086, 92}, {3285, 63}}
	spans := []observer.SpanInfo{
		{SpanID: "workflow", SpanName: "LangGraph.workflow", ParentSpanID: "root", StartTime: start},
	}
	details := map[string]*observer.SpanDetailsResponse{
		"workflow": {
			SpanID: "workflow", SpanName: "LangGraph.workflow",
			Attributes: map[string]interface{}{
				"traceloop.entity.input":  `{"inputs":"reset my password"}`,
				"traceloop.entity.output": workflowOutput,
			},
		},
	}
	for i, u := range leafUsage {
		id := fmt.Sprintf("chat-%d", i)
		seq := fmt.Sprintf("seq-%d", i)
		spans = append(spans, observer.SpanInfo{
			SpanID: id, SpanName: "ChatOpenAI.chat", ParentSpanID: seq,
			StartTime: start.Add(time.Duration(i+1) * time.Second),
		})
		details[id] = &observer.SpanDetailsResponse{
			SpanID: id, SpanName: "ChatOpenAI.chat", ParentSpanID: seq,
			Attributes: map[string]interface{}{
				"gen_ai.usage.input_tokens":  float64(u[0]),
				"gen_ai.usage.output_tokens": float64(u[1]),
			},
		}
	}
	fake := &fakeObserverClient{spans: spans, spanDetails: details}
	c := NewTracingController(fake)

	input, output, tokens := c.enrichTraceOverview(context.Background(), baseParams(), baseTraceInfo(24), root, testFetchSem())

	if input == nil || output == nil {
		t.Errorf("expected input/output from workflow span, got input=%v output=%v", input, output)
	}
	if tokens == nil || tokens.InputTokens != 9353 || tokens.OutputTokens != 186 || tokens.TotalTokens != 9539 {
		t.Errorf("tokens = %+v, want sum of the three LLM calls (9353/186/9539)", tokens)
	}
}

// (d) Everything empty — degenerate case. No extra fetches beyond QueryTraceSpans.
// All three return nil; trace overview row simply renders "-" in the UI.
func TestEnrichTraceOverview_AllEmptyReturnsNil(t *testing.T) {
	root := makeRootSpan("root", map[string]interface{}{})
	fake := &fakeObserverClient{spans: []observer.SpanInfo{}}
	c := NewTracingController(fake)

	input, output, tokens := c.enrichTraceOverview(context.Background(), baseParams(), baseTraceInfo(1), root, testFetchSem())

	if input != nil || output != nil || tokens != nil {
		t.Errorf("expected all nil, got input=%v output=%v tokens=%+v", input, output, tokens)
	}
}

// (e) Leaf cap honoured. With > maxLLMLeavesPerTrace leaves, only the cap is
// fetched and TokenUsage.Partial is true.
func TestEnrichTraceOverview_LeafCapHonoredAndPartialFlagged(t *testing.T) {
	root := makeRootSpan("root", map[string]interface{}{})
	const totalLeaves = maxLLMLeavesPerTrace + 5

	spans := make([]observer.SpanInfo, 0, totalLeaves)
	details := make(map[string]*observer.SpanDetailsResponse, totalLeaves)
	start := time.Now().Add(-1 * time.Hour)
	for i := 0; i < totalLeaves; i++ {
		id := fmt.Sprintf("leaf-%02d", i)
		spans = append(spans, observer.SpanInfo{
			SpanID: id, SpanName: "openai.chat", ParentSpanID: "root",
			StartTime: start.Add(time.Duration(i) * time.Second),
		})
		details[id] = &observer.SpanDetailsResponse{
			SpanID: id, SpanName: "openai.chat",
			Attributes: map[string]interface{}{
				"gen_ai.input.messages":      `[{"role":"user","content":"u"}]`,
				"gen_ai.output.messages":     `[{"role":"assistant","content":"a"}]`,
				"gen_ai.usage.input_tokens":  float64(1),
				"gen_ai.usage.output_tokens": float64(1),
			},
		}
	}
	fake := &fakeObserverClient{spans: spans, spanDetails: details}
	c := NewTracingController(fake)

	// SpanCount stays clearly under the skip threshold so step 3 runs; the
	// cap (maxLLMLeavesPerTrace) is what should trigger Partial. Using
	// threshold-1 expresses "below the skip threshold" independently of
	// whether the guard ever tightens from > to >=.
	_, _, tokens := c.enrichTraceOverview(context.Background(), baseParams(), baseTraceInfo(skipLeafAggregationSpanCountThreshold-1), root, testFetchSem())

	if tokens == nil {
		t.Fatalf("expected tokens, got nil")
	}
	if !tokens.Partial {
		t.Errorf("expected Partial=true when leaves exceed cap")
	}
	if tokens.TotalTokens != maxLLMLeavesPerTrace*2 {
		t.Errorf("expected sum from %d leaves (cap), got TotalTokens=%d", maxLLMLeavesPerTrace, tokens.TotalTokens)
	}
	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != int32(maxLLMLeavesPerTrace) {
		t.Errorf("expected exactly %d GetSpanDetails fetches, got %d", maxLLMLeavesPerTrace, got)
	}
}

// Cost guard: when the trace's total span count exceeds the skip threshold,
// step 3 is bypassed entirely — no leaf fetches happen even if leaves exist.
func TestEnrichTraceOverview_SkipsLeafAggregationForHugeTraces(t *testing.T) {
	root := makeRootSpan("root", map[string]interface{}{})
	fake := &fakeObserverClient{
		spans: []observer.SpanInfo{
			{SpanID: "leaf-1", SpanName: "openai.chat", ParentSpanID: "root", StartTime: time.Now()},
		},
		spanDetails: map[string]*observer.SpanDetailsResponse{
			"leaf-1": {SpanID: "leaf-1", Attributes: map[string]interface{}{"gen_ai.usage.input_tokens": float64(1)}},
		},
	}
	c := NewTracingController(fake)

	hugeTrace := baseTraceInfo(skipLeafAggregationSpanCountThreshold + 1)
	input, output, tokens := c.enrichTraceOverview(context.Background(), baseParams(), hugeTrace, root, testFetchSem())

	if input != nil || output != nil || tokens != nil {
		t.Errorf("expected nil for huge trace, got input=%v output=%v tokens=%+v", input, output, tokens)
	}
	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != 0 {
		t.Errorf("expected no GetSpanDetails calls (skip), got %d", got)
	}
}

// TestExportTraces_UsesBulkAttributes verifies the export path fetches span
// attributes inline via QueryTraceSpans(includeAttributes=true) and makes zero
// GetSpanDetails calls, while preserving span kind/status/attributes.
func TestExportTraces_UsesBulkAttributes(t *testing.T) {
	now := time.Now()
	rootAttrs := map[string]interface{}{
		"traceloop.entity.input":  `{"inputs":"hello"}`,
		"traceloop.entity.output": `{"outputs":{"messages":[{"kwargs":{"content":"hi"}}]}}`,
	}
	fake := &fakeObserverClient{
		traces: []observer.TraceInfo{{
			TraceID:    "trace-1",
			RootSpanID: "root",
			StartTime:  now.Add(-time.Minute),
			EndTime:    now,
			SpanCount:  2,
		}},
		spans: []observer.SpanInfo{
			{SpanID: "root", SpanName: "invoke_agent", Kind: "SERVER", Status: &observer.SpanStatus{Code: "ok"}, StartTime: now.Add(-time.Minute), EndTime: now, Attributes: rootAttrs},
			{SpanID: "child", SpanName: "llm", ParentSpanID: "root", Kind: "INTERNAL", Status: &observer.SpanStatus{Code: "ok"}, StartTime: now.Add(-30 * time.Second), EndTime: now},
		},
	}
	c := NewTracingController(fake)

	resp, err := c.ExportTraces(context.Background(), baseParams())
	if err != nil {
		t.Fatalf("ExportTraces returned error: %v", err)
	}
	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != 0 {
		t.Errorf("expected 0 GetSpanDetails calls, got %d", got)
	}
	if !fake.lastSpansReq.IncludeAttributes {
		t.Error("expected QueryTraceSpans to request IncludeAttributes=true")
	}
	if len(resp.Traces) != 1 {
		t.Fatalf("expected 1 exported trace, got %d", len(resp.Traces))
	}
	tr := resp.Traces[0]
	if len(tr.Spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(tr.Spans))
	}
	var root *opensearch.Span
	for i := range tr.Spans {
		if tr.Spans[i].SpanID == "root" {
			root = &tr.Spans[i]
		}
	}
	if root == nil {
		t.Fatal("root span missing from export")
	}
	if root.Kind != "SERVER" {
		t.Errorf("expected root Kind SERVER, got %q", root.Kind)
	}
	if _, ok := root.Attributes["traceloop.entity.input"]; !ok {
		t.Error("expected root span to retain bulk-fetched attributes")
	}
}
