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
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wso2/agent-manager/agent-manager-observer/observer"
	"github.com/wso2/agent-manager/agent-manager-observer/opensearch"
)

// judgeAttrs is a judge's span attributes: a model, tokens, messages, a
// conversation, error.type skipped and this run ID.
func judgeAttrs(runID string) map[string]interface{} {
	return map[string]interface{}{
		opensearch.EvalRunIDAttribute:   runID,
		"amp.monitor.id":                "monitor-1",
		"gen_ai.evaluation.name":        "Hallucination",
		"gen_ai.evaluation.score.value": 0.7,
		"error.type":                    "skipped",
		"gen_ai.operation.name":         "chat",
		"gen_ai.request.model":          "judge-model",
		"gen_ai.usage.input_tokens":     float64(500),
		"gen_ai.usage.output_tokens":    float64(50),
		"gen_ai.conversation.id":        "judge-conversation",
		"gen_ai.input.messages":         `[{"role":"user","parts":[{"type":"text","content":"judge in"}]}]`,
		"gen_ai.output.messages":        `[{"role":"assistant","parts":[{"type":"text","content":"judge out"}]}]`,
	}
}

// withRootChildren adds spans under chainSpanFake's root, one per name, that
// start first and last 1 µs, as the evaluation job's do.
func withRootChildren(fake *fakeObserverClient, attrs map[string]interface{}, names ...string) *fakeObserverClient {
	start := fake.spans[0].StartTime.Add(-time.Second)
	added := make([]observer.SpanInfo, 0, len(names))
	for i, name := range names {
		id := fmt.Sprintf("added-%d", i)
		s := observer.SpanInfo{
			SpanID: id, SpanName: name, ParentSpanID: "root", Kind: "INTERNAL",
			StartTime: start, EndTime: start.Add(time.Microsecond), DurationNs: 1000,
			Status: &observer.SpanStatus{Code: "Error"}, Attributes: attrs,
		}
		added = append(added, s)
		fake.spanDetails[id] = &observer.SpanDetailsResponse{
			SpanID: id, SpanName: name, ParentSpanID: "root", Kind: "INTERNAL",
			StartTime: s.StartTime, EndTime: s.EndTime, DurationNs: s.DurationNs,
			Status: s.Status, Attributes: attrs,
		}
	}
	// The root stays last, as upstream lists it.
	fake.spans = append(added, fake.spans...)
	fake.traces[0].SpanCount = len(fake.spans)
	return fake
}

// sameTrace returns a fresh fake that scripts fake's trace.
func sameTrace(fake *fakeObserverClient) *fakeObserverClient {
	return &fakeObserverClient{
		traces:      slices.Clone(fake.traces),
		rootSpan:    fake.rootSpan,
		spans:       slices.Clone(fake.spans),
		spanDetails: maps.Clone(fake.spanDetails),
	}
}

// scoredChainFake is plain's trace with evaluation spans named to pass for a
// chain, an LLM call and a tool.
func scoredChainFake(plain *fakeObserverClient) *fakeObserverClient {
	return withRootChildren(sameTrace(plain), judgeAttrs("run-1"),
		"evaluation Hallucination", "evaluation judge.chat", "evaluation lookup.tool")
}

// A trace's list row, filters and export are the same with evaluation spans as without.
func TestEvaluationSpans_ListAndExportUnchanged(t *testing.T) {
	includes := map[string]Include{
		"no include":  {},
		"all include": {Models: true, Tools: true, MCPServers: true},
	}
	for name, include := range includes {
		t.Run("list/"+name, func(t *testing.T) {
			params := baseParams()
			params.Include = include

			plain := chainSpanFake()
			want := singleOverview(t, NewTracingController(plain), params)
			got := singleOverview(t, NewTracingController(scoredChainFake(plain)), params)

			// The list keeps upstream's span count.
			if got.SpanCount != want.SpanCount+3 {
				t.Errorf("spanCount = %d, want %d", got.SpanCount, want.SpanCount+3)
			}
			got.SpanCount = want.SpanCount
			if !reflect.DeepEqual(got, want) {
				t.Errorf("overview with evaluation spans =\n%+v\nwant\n%+v", got, want)
			}
		})
	}

	filters := map[string]TraceFilters{
		"model":           {Model: "judge-model"},
		"tool":            {Tool: "evaluation lookup"},
		"toolError":       {ToolError: true},
		"conversationId":  {ConversationID: "judge-conversation"},
		"minTokens":       {MinTokens: ptr(500)},
		"status error":    {Status: TraceStatusError},
		"matching tokens": {MinTokens: ptr(25)},
	}
	for name, f := range filters {
		t.Run("filter/"+name, func(t *testing.T) {
			params := baseParams()
			params.Filters = f

			plain := chainSpanFake()
			want, _, _ := traceIDs(NewTracingController(plain), t, params)
			got, _, _ := traceIDs(NewTracingController(scoredChainFake(plain)), t, params)

			if !slices.Equal(got, want) {
				t.Errorf("traces = %v, want %v", got, want)
			}
		})
	}

	t.Run("export", func(t *testing.T) {
		plain := chainSpanFake()
		want, err := NewTracingController(plain).ExportTraces(context.Background(), baseParams())
		if err != nil {
			t.Fatalf("ExportTraces returned error: %v", err)
		}
		got, err := NewTracingController(scoredChainFake(plain)).ExportTraces(context.Background(), baseParams())
		if err != nil {
			t.Fatalf("ExportTraces returned error: %v", err)
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("export with evaluation spans =\n%s\nwant\n%s", gotJSON, wantJSON)
		}
	})
}

// The span tree leaves evaluation spans out of its spans and total. It checks
// the spans whose names may be evaluation spans' by their details, or past
// maxEvaluationSpanChecks by fetching the list again with attributes.
func TestEvaluationSpans_SpanTree(t *testing.T) {
	type calls struct{ lists, attrLists, details int32 }
	manyNames := make([]string, maxEvaluationSpanChecks+1)
	for i := range manyNames {
		manyNames[i] = fmt.Sprintf("evaluation Evaluator %d", i)
	}
	tests := []struct {
		name  string
		names []string
		want  calls
	}{
		{name: "no evaluation spans", want: calls{lists: 1}},
		{name: "evaluation spans", names: []string{"evaluation Hallucination", "evaluation judge.chat", "evaluation lookup.tool"},
			want: calls{lists: 1, details: 3}},
		{name: "past the check cap", names: manyNames, want: calls{lists: 2, attrLists: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plain := chainSpanFake()
			want, err := NewTracingController(plain).GetTraceSpans(context.Background(), "trace-1", baseParams())
			if err != nil {
				t.Fatalf("GetTraceSpans returned error: %v", err)
			}
			scored := sameTrace(plain)
			if tt.names != nil {
				scored = withRootChildren(scored, judgeAttrs("run-1"), tt.names...)
			}

			got, err := NewTracingController(scored).GetTraceSpans(context.Background(), "trace-1", baseParams())
			if err != nil {
				t.Fatalf("GetTraceSpans returned error: %v", err)
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("span tree =\n%+v\nwant\n%+v", got, want)
			}
			gotCalls := calls{
				lists:     atomic.LoadInt32(&scored.queryTraceSpansCalls),
				attrLists: atomic.LoadInt32(&scored.attrSpansCalls),
				details:   atomic.LoadInt32(&scored.getSpanDetailsCalls),
			}
			if gotCalls != tt.want {
				t.Errorf("calls = %+v, want %+v", gotCalls, tt.want)
			}
		})
	}
}

// When a span can't be checked, the span tree fails rather than show it.
func TestEvaluationSpans_SpanTreeFailsWhenUnchecked(t *testing.T) {
	fake := scoredChainFake(chainSpanFake())
	fake.failCall = func(id string) error {
		if id == "added-1" {
			return errors.New("fake: span details failed")
		}
		return nil
	}
	if _, err := NewTracingController(fake).GetTraceSpans(context.Background(), "trace-1", baseParams()); err == nil {
		t.Fatal("GetTraceSpans returned no error, want the failed check's")
	}
}

// Span details of an evaluation span are ErrSpanNotFound.
func TestEvaluationSpans_SpanDetail(t *testing.T) {
	c := NewTracingController(scoredChainFake(chainSpanFake()))

	if _, err := c.GetSpanDetail(context.Background(), "trace-1", "added-0"); !errors.Is(err, ErrSpanNotFound) {
		t.Errorf("GetSpanDetail(evaluation span) error = %v, want ErrSpanNotFound", err)
	}
	if span, err := c.GetSpanDetail(context.Background(), "trace-1", "chain-1"); err != nil || span.SpanID != "chain-1" {
		t.Errorf("GetSpanDetail(chain-1) = %v, %v, want the span", span, err)
	}
}

// Spans with gen_ai.evaluation.* and no run ID, such as an agent's inline
// DeepEval results, are ordinary spans everywhere.
func TestEvaluationSpans_WithoutRunIDStayVisible(t *testing.T) {
	deepEval := judgeAttrs("")
	delete(deepEval, opensearch.EvalRunIDAttribute)
	delete(deepEval, "amp.monitor.id")
	cases := map[string]map[string]interface{}{
		"inline DeepEval span": deepEval,
		"empty run ID":         judgeAttrs(""),
	}
	for name, attrs := range cases {
		t.Run(name, func(t *testing.T) {
			fake := withRootChildren(chainSpanFake(), attrs, "evaluation Relevance")
			c := NewTracingController(fake)

			tree, err := c.GetTraceSpans(context.Background(), "trace-1", baseParams())
			if err != nil {
				t.Fatalf("GetTraceSpans returned error: %v", err)
			}
			if tree.TotalCount != 5 || !slices.ContainsFunc(tree.Spans, func(s SpanSummary) bool { return s.SpanID == "added-0" && s.Error }) {
				t.Errorf("span tree = %+v, want the span, failed, among 5", tree)
			}

			if _, err := c.GetSpanDetail(context.Background(), "trace-1", "added-0"); err != nil {
				t.Errorf("GetSpanDetail returned error: %v", err)
			}

			export, err := c.ExportTraces(context.Background(), baseParams())
			if err != nil {
				t.Fatalf("ExportTraces returned error: %v", err)
			}
			tr := export.Traces[0]
			if tr.SpanCount != 5 || len(tr.Spans) != 5 {
				t.Errorf("export spanCount = %d with %d spans, want 5", tr.SpanCount, len(tr.Spans))
			}
			// Its error.type marks an error; its tokens count, beside the chain span's 25.
			if tr.Status == nil || tr.Status.ErrorCount != 1 {
				t.Errorf("export status = %+v, want 1 error", tr.Status)
			}
			if tr.TokenUsage == nil || tr.TokenUsage.TotalTokens != 575 {
				t.Errorf("export tokens = %+v, want 575", tr.TokenUsage)
			}

			// It starts first under the root, so the list reads it as the chain span.
			if ov := singleOverview(t, c, baseParams()); ov.ConversationID != "judge-conversation" {
				t.Errorf("list conversationId = %q, want the span's", ov.ConversationID)
			}
		})
	}
}
