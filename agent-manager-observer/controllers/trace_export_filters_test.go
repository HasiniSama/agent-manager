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
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wso2/agent-manager/agent-manager-observer/opensearch"
)

func exportParams(limit int, f TraceFilters) TraceQueryParams {
	params := lookBackParams(limit)
	params.Filters = f
	return params
}

func exportedIDs(resp *opensearch.TraceExportResponse) []string {
	ids := make([]string, 0, len(resp.Traces))
	for _, tr := range resp.Traces {
		ids = append(ids, tr.TraceID)
	}
	return ids
}

func mustExport(t *testing.T, c *TracingController, params TraceQueryParams) *opensearch.TraceExportResponse {
	t.Helper()
	resp, err := c.ExportTraces(context.Background(), params)
	if err != nil {
		t.Fatalf("ExportTraces returned error: %v", err)
	}
	return resp
}

// status=error fetches full spans only for the error traces.
func TestExportTraces_StatusErrorFetchesSpansOnlyForMatches(t *testing.T) {
	fake := langGraphFake(60, errorEvery(10))
	c := NewTracingController(fake)

	resp := mustExport(t, c, exportParams(100, TraceFilters{Status: TraceStatusError}))

	want := []string{"trace-0000", "trace-0010", "trace-0020", "trace-0030", "trace-0040", "trace-0050"}
	if got := exportedIDs(resp); !reflect.DeepEqual(got, want) {
		t.Fatalf("exported %v, want %v", got, want)
	}
	if got := atomic.LoadInt32(&fake.attrSpansCalls); got != int32(len(want)) {
		t.Errorf("full span fetches = %d, want %d (one per match)", got, len(want))
	}
	if resp.TotalCount != len(want) || resp.Truncated {
		t.Errorf("totalCount %d, truncated %v; want %d, false", resp.TotalCount, resp.Truncated, len(want))
	}
	if wantEdge := formatCursor(lookBackWindowEnd.Add(-24 * time.Hour)); resp.LookedBackTo != wantEdge {
		t.Errorf("lookedBackTo = %q, want the window edge %q", resp.LookedBackTo, wantEdge)
	}
	for i, tr := range resp.Traces {
		wantStart := fake.traces[i*10].StartTime.Format(time.RFC3339Nano)
		if tr.StartTime != wantStart {
			t.Errorf("%s startTime = %q, want %q", tr.TraceID, tr.StartTime, wantStart)
		}
		if len(tr.Spans) != 4 {
			t.Errorf("%s has %d spans, want 4", tr.TraceID, len(tr.Spans))
		}
	}
}

// Without filters the export makes one QueryTraces and one span fetch per trace, as before.
func TestExportTraces_NoFilterUnchanged(t *testing.T) {
	fake := langGraphFake(60, noRootAttrs)
	c := NewTracingController(fake)

	resp := mustExport(t, c, exportParams(20, TraceFilters{}))

	if len(resp.Traces) != 20 {
		t.Fatalf("exported %d traces, want 20", len(resp.Traces))
	}
	if got := atomic.LoadInt32(&fake.queryTracesCalls); got != 1 {
		t.Errorf("QueryTraces calls = %d, want 1", got)
	}
	if got := fake.tracesReqs[0].Limit; got == nil || *got != 20 {
		t.Errorf("QueryTraces limit = %v, want 20", got)
	}
	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != 0 {
		t.Errorf("GetSpanDetails calls = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&fake.attrSpansCalls); got != 20 {
		t.Errorf("full span fetches = %d, want 20", got)
	}
	if resp.TotalCount != 60 || resp.Truncated || resp.LookedBackTo != "" {
		t.Errorf("totalCount %d, truncated %v, lookedBackTo %q; want 60, false, empty",
			resp.TotalCount, resp.Truncated, resp.LookedBackTo)
	}
}

// A rare filter stops at the examine cap and fetches no full spans for non-matches.
func TestExportTraces_StopsAtExamineCap(t *testing.T) {
	fake := langGraphFake(maxExaminedTraces+100, noRootAttrs)
	c := NewTracingController(fake)

	resp := mustExport(t, c, exportParams(100, TraceFilters{Status: TraceStatusError}))

	if len(resp.Traces) != 0 || !resp.Truncated {
		t.Fatalf("exported %d traces, truncated %v; want 0, true", len(resp.Traces), resp.Truncated)
	}
	if got := atomic.LoadInt32(&fake.attrSpansCalls); got != 0 {
		t.Errorf("full span fetches = %d, want 0", got)
	}
	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != maxExaminedTraces {
		t.Errorf("GetSpanDetails calls = %d, want %d (one root per examined trace)", got, maxExaminedTraces)
	}
	if want := formatCursor(fake.traces[maxExaminedTraces-1].StartTime); resp.LookedBackTo != want {
		t.Errorf("lookedBackTo = %q, want the last examined trace %q", resp.LookedBackTo, want)
	}
}

// A model filter selects by models without the caller setting Include.Models.
func TestExportTraces_ModelFilter(t *testing.T) {
	fake := langGraphFake(20, noRootAttrs)
	c := NewTracingController(fake)

	resp := mustExport(t, c, exportParams(3, TraceFilters{Model: "claude"}))

	want := []string{"trace-0001", "trace-0003", "trace-0005"}
	if got := exportedIDs(resp); !reflect.DeepEqual(got, want) {
		t.Fatalf("exported %v, want %v", got, want)
	}
	for _, tr := range resp.Traces {
		var models []string
		for _, s := range tr.Spans {
			if m, ok := s.Attributes["gen_ai.response.model"].(string); ok {
				models = append(models, m)
			}
		}
		if len(models) == 0 || models[0] != "claude-sonnet-4-5" {
			t.Errorf("%s models = %v, want claude-sonnet-4-5", tr.TraceID, models)
		}
	}
}

// The export's filtered selection matches the list's first page for the same filters.
func TestExportTraces_SelectsSameTracesAsList(t *testing.T) {
	filters := map[string]TraceFilters{
		"status=error":      {Status: TraceStatusError},
		"status=ok":         {Status: TraceStatusOK},
		"minTokens=40":      {MinTokens: ptr(40)},
		"model=gpt-4o-mini": {Model: "gpt-4o-mini"},
	}
	for name, f := range filters {
		t.Run(name, func(t *testing.T) {
			c := NewTracingController(langGraphFake(80, errorEvery(7)))
			params := exportParams(15, f)

			list, err := c.GetTraceOverviews(context.Background(), params)
			if err != nil {
				t.Fatalf("GetTraceOverviews returned error: %v", err)
			}
			listIDs := make([]string, 0, len(list.Traces))
			for _, ov := range list.Traces {
				listIDs = append(listIDs, ov.TraceID)
			}
			resp := mustExport(t, c, params)

			if got := exportedIDs(resp); !reflect.DeepEqual(got, listIDs) {
				t.Errorf("export selected %v, list returned %v", got, listIDs)
			}
			if resp.LookedBackTo != list.LookedBackTo || resp.Truncated != list.Truncated {
				t.Errorf("export lookedBackTo %q truncated %v, list %q %v",
					resp.LookedBackTo, resp.Truncated, list.LookedBackTo, list.Truncated)
			}
		})
	}
}

// A filtered export that runs out of time exports the matches selected so far
// and fetches full spans only for them. The list's budget would not stop it.
func TestExportTraces_StopsAtTimeBudget(t *testing.T) {
	fake := langGraphFake(600, errorEvery(30))
	c := NewTracingController(fake)
	advanceOnRoot(fake, withClock(c), "root-0050", exportLookBackBudget)
	ctx, logs := logContext()

	resp, err := c.ExportTraces(ctx, exportParams(100, TraceFilters{Status: TraceStatusError}))
	if err != nil {
		t.Fatalf("ExportTraces returned error: %v", err)
	}

	want := []string{"trace-0000", "trace-0030", "trace-0060", "trace-0090"}
	if got := exportedIDs(resp); !reflect.DeepEqual(got, want) {
		t.Fatalf("exported %v, want %v", got, want)
	}
	if !resp.Truncated {
		t.Error("truncated = false, want true")
	}
	if want := formatCursor(fake.traces[99].StartTime); resp.LookedBackTo != want {
		t.Errorf("lookedBackTo = %q, want the last examined trace %q", resp.LookedBackTo, want)
	}
	if got := atomic.LoadInt32(&fake.attrSpansCalls); got != int32(len(want)) {
		t.Errorf("full span fetches = %d, want %d (one per match)", got, len(want))
	}
	for _, id := range fake.spansTraceIDs {
		if !slices.Contains(want, id) {
			t.Errorf("QueryTraceSpans called for %s, which was not selected", id)
		}
	}
	if got := logField(t, logs, "Selected traces for export", "budgetExceeded"); got != true {
		t.Errorf("budgetExceeded logged as %v, want true", got)
	}
}

// A trace over the span cap sets spansTruncated, and truncated with it, with or without a filter.
func TestExportTraces_SpanCapSetsSpansTruncated(t *testing.T) {
	filters := map[string]TraceFilters{
		"no filter":    {},
		"status=error": {Status: TraceStatusError},
	}
	for name, f := range filters {
		t.Run(name, func(t *testing.T) {
			fake := langGraphFake(60, errorEvery(10))
			fake.traces[10].SpanCount = MaxSpansPerRequest + 1
			c := NewTracingController(fake)
			ctx, logs := logContext()

			resp, err := c.ExportTraces(ctx, exportParams(100, f))
			if err != nil {
				t.Fatalf("ExportTraces returned error: %v", err)
			}

			if !slices.Contains(exportedIDs(resp), "trace-0010") {
				t.Fatalf("exported %v, want trace-0010 among them", exportedIDs(resp))
			}
			if !resp.SpansTruncated || !resp.Truncated {
				t.Errorf("spansTruncated %v, truncated %v; want both true", resp.SpansTruncated, resp.Truncated)
			}
			if got := logField(t, logs, "Completed trace export", "spansTruncated"); got != true {
				t.Errorf("spansTruncated logged as %v, want true", got)
			}
		})
	}
}

// A filtered export stopped by the examine cap is truncated without spansTruncated.
func TestExportTraces_ExamineCapLeavesSpansTruncatedFalse(t *testing.T) {
	fake := langGraphFake(maxExaminedTraces+100, errorEvery(100))
	c := NewTracingController(fake)

	resp := mustExport(t, c, exportParams(100, TraceFilters{Status: TraceStatusError}))

	want := []string{"trace-0000", "trace-0100", "trace-0200", "trace-0300", "trace-0400"}
	if got := exportedIDs(resp); !reflect.DeepEqual(got, want) {
		t.Fatalf("exported %v, want %v", got, want)
	}
	if !resp.Truncated || resp.SpansTruncated {
		t.Errorf("truncated %v, spansTruncated %v; want true, false", resp.Truncated, resp.SpansTruncated)
	}
}

// An export that covers the window with every trace under the span cap sets neither flag.
func TestExportTraces_CompleteExportSetsNeither(t *testing.T) {
	filters := map[string]TraceFilters{
		"no filter":    {},
		"status=error": {Status: TraceStatusError},
	}
	for name, f := range filters {
		t.Run(name, func(t *testing.T) {
			c := NewTracingController(langGraphFake(60, errorEvery(10)))

			resp := mustExport(t, c, exportParams(100, f))

			if len(resp.Traces) == 0 {
				t.Fatal("exported no traces")
			}
			if resp.Truncated || resp.SpansTruncated {
				t.Errorf("truncated %v, spansTruncated %v; want both false", resp.Truncated, resp.SpansTruncated)
			}
		})
	}
}

// spansTruncated is always in the JSON, so a client can read false without a default.
func TestTraceExportResponse_SpansTruncatedAlwaysPresent(t *testing.T) {
	b, err := json.Marshal(opensearch.TraceExportResponse{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"spansTruncated":false`) {
		t.Errorf("marshalled %s, want spansTruncated false", b)
	}
}
