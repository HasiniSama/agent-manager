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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wso2/agent-manager/agent-manager-observer/middleware/logger"
	"github.com/wso2/agent-manager/agent-manager-observer/observer"
)

var lookBackWindowEnd = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// lookBackFake scripts n zero-length traces one second apart, newest first,
// inside a one-day window. Every matchEvery-th trace (from 0) carries
// conversation ID conv-match.
func lookBackFake(n, matchEvery int) *fakeObserverClient {
	fake := &fakeObserverClient{windowed: true, spanDetails: map[string]*observer.SpanDetailsResponse{}}
	for i := 0; i < n; i++ {
		info := baseTraceInfo(2)
		info.TraceID = fmt.Sprintf("trace-%04d", i)
		info.RootSpanID = fmt.Sprintf("root-%04d", i)
		info.StartTime = lookBackWindowEnd.Add(-time.Duration(i+1) * time.Second)
		info.EndTime = info.StartTime
		fake.traces = append(fake.traces, info)

		attrs := completeRootAttrs()
		attrs["gen_ai.conversation.id"] = "conv-other"
		if i%matchEvery == 0 {
			attrs["gen_ai.conversation.id"] = "conv-match"
		}
		fake.spanDetails[info.RootSpanID] = &observer.SpanDetailsResponse{
			SpanID: info.RootSpanID, SpanName: "invoke_agent LangGraph", Attributes: attrs,
		}
	}
	return fake
}

// lookBackParams builds a 24h window filtered on conv-match.
func lookBackParams(limit int) TraceQueryParams {
	params := baseParams()
	params.StartTime = lookBackWindowEnd.Add(-24 * time.Hour)
	params.EndTime = lookBackWindowEnd
	params.Limit = limit
	params.Filters.ConversationID = "conv-match"
	return params
}

// assertNoDuplicates fails if a trace ID appears twice.
func assertNoDuplicates(t *testing.T, ids []string) {
	t.Helper()
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("trace %s returned twice", id)
		}
		seen[id] = true
	}
}

// traceIDs fetches one page and returns its IDs, lookedBackTo and truncated.
func traceIDs(c *TracingController, t *testing.T, params TraceQueryParams) ([]string, string, bool) {
	t.Helper()
	resp, err := c.GetTraceOverviews(context.Background(), params)
	if err != nil {
		t.Fatalf("GetTraceOverviews returned error: %v", err)
	}
	if !params.Filters.IsZero() && resp.TotalCount != len(resp.Traces) {
		t.Errorf("totalCount = %d, want %d", resp.TotalCount, len(resp.Traces))
	}
	ids := make([]string, len(resp.Traces))
	for i, tr := range resp.Traces {
		ids[i] = tr.TraceID
	}
	return ids, resp.LookedBackTo, resp.Truncated
}

// No filter: one QueryTraces call for the requested limit.
func TestGetTraceOverviews_NoFilterQueriesOnce(t *testing.T) {
	fake := lookBackFake(200, 5)
	c := NewTracingController(fake)
	params := lookBackParams(20)
	params.Filters = TraceFilters{}

	ids, lookedBackTo, truncated := traceIDs(c, t, params)

	if got := atomic.LoadInt32(&fake.queryTracesCalls); got != 1 {
		t.Fatalf("QueryTraces calls = %d, want 1", got)
	}
	if got := *fake.tracesReqs[0].Limit; got != 20 {
		t.Errorf("QueryTraces limit = %d, want 20", got)
	}
	if len(ids) != 20 || truncated {
		t.Errorf("got %d traces, truncated %v; want 20, false", len(ids), truncated)
	}
	if want := formatCursor(fake.traces[19].StartTime); lookedBackTo != want {
		t.Errorf("lookedBackTo = %s, want %s", lookedBackTo, want)
	}
}

// A 1-in-5 filter keeps looking back until the page is full.
func TestGetTraceOverviews_LookBackFillsPage(t *testing.T) {
	fake := lookBackFake(400, 5)
	c := NewTracingController(fake)

	ids, lookedBackTo, truncated := traceIDs(c, t, lookBackParams(25))

	if len(ids) != 25 || truncated {
		t.Fatalf("got %d traces, truncated %v; want 25, false", len(ids), truncated)
	}
	for i, id := range ids {
		if want := fmt.Sprintf("trace-%04d", i*5); id != want {
			t.Fatalf("traces[%d] = %s, want %s", i, id, want)
		}
	}
	// The 25th match is trace 120, in the third fetch.
	if want := formatCursor(fake.traces[120].StartTime); lookedBackTo != want {
		t.Errorf("lookedBackTo = %s, want %s", lookedBackTo, want)
	}
	wantLimits := []int{50, 100, 200}
	if len(fake.tracesReqs) != len(wantLimits) {
		t.Fatalf("QueryTraces calls = %d, want %d", len(fake.tracesReqs), len(wantLimits))
	}
	for i, req := range fake.tracesReqs {
		if *req.Limit != wantLimits[i] {
			t.Errorf("fetch %d limit = %d, want %d", i, *req.Limit, wantLimits[i])
		}
		if !req.StartTime.Equal(fake.tracesReqs[0].StartTime) || !req.EndTime.Equal(fake.tracesReqs[0].EndTime) {
			t.Errorf("fetch %d window = %s..%s, want the request window", i, req.StartTime, req.EndTime)
		}
	}
}

// A filter that never matches stops at the examine cap.
func TestGetTraceOverviews_LookBackStopsAtCap(t *testing.T) {
	fake := lookBackFake(1000, 5)
	c := NewTracingController(fake)
	params := lookBackParams(10)
	params.Filters.ConversationID = "conv-none"

	ids, lookedBackTo, truncated := traceIDs(c, t, params)

	if len(ids) != 0 || !truncated {
		t.Fatalf("got %d traces, truncated %v; want 0, true", len(ids), truncated)
	}
	if want := formatCursor(fake.traces[maxExaminedTraces-1].StartTime); lookedBackTo != want {
		t.Errorf("lookedBackTo = %s, want %s", lookedBackTo, want)
	}
	// Each examined trace costs one root fetch, so the cap bounds enrichment.
	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != maxExaminedTraces {
		t.Errorf("GetSpanDetails calls = %d, want %d", got, maxExaminedTraces)
	}
}

// Running out of window before the cap is not truncation.
func TestGetTraceOverviews_LookBackWindowExhausted(t *testing.T) {
	tests := []struct {
		name      string
		sortOrder string
		wantEdge  func(TraceQueryParams) time.Time
	}{
		{name: "desc", sortOrder: "desc", wantEdge: func(p TraceQueryParams) time.Time { return p.StartTime }},
		{name: "asc", sortOrder: "asc", wantEdge: func(p TraceQueryParams) time.Time { return p.EndTime }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := lookBackFake(120, 5)
			c := NewTracingController(fake)
			params := lookBackParams(100)
			params.SortOrder = tt.sortOrder

			ids, lookedBackTo, truncated := traceIDs(c, t, params)

			if len(ids) != 24 || truncated {
				t.Fatalf("got %d traces, truncated %v; want 24, false", len(ids), truncated)
			}
			if want := formatCursor(tt.wantEdge(params)); lookedBackTo != want {
				t.Errorf("lookedBackTo = %s, want %s", lookedBackTo, want)
			}
		})
	}
}

// Ascending order pages forward, so lookedBackTo is the newest trace examined.
func TestGetTraceOverviews_LookBackAscending(t *testing.T) {
	fake := lookBackFake(400, 5)
	c := NewTracingController(fake)
	params := lookBackParams(25)
	params.SortOrder = "asc"

	ids, lookedBackTo, _ := traceIDs(c, t, params)

	if len(ids) != 25 || ids[0] != "trace-0395" {
		t.Fatalf("got %d traces starting at %v; want 25 starting at trace-0395", len(ids), ids)
	}
	// Oldest-first matches are 395, 390, ...; the 25th is trace 275.
	if want := formatCursor(fake.traces[275].StartTime); lookedBackTo != want {
		t.Errorf("lookedBackTo = %s, want %s", lookedBackTo, want)
	}
}

// Traces sharing a timestamp at a fetch boundary are neither lost nor repeated.
func TestGetTraceOverviews_LookBackSharedBoundaryTimestamp(t *testing.T) {
	fake := lookBackFake(120, 1)
	// Traces 45-54 straddle the first fetch boundary at one timestamp.
	for i := 45; i < 55; i++ {
		fake.traces[i].StartTime = fake.traces[45].StartTime
	}
	c := NewTracingController(fake)

	ids, _, _ := traceIDs(c, t, lookBackParams(200))

	assertNoDuplicates(t, ids)
	if len(ids) != 120 {
		t.Errorf("got %d traces, want all 120", len(ids))
	}
}

// More than a fetch at one timestamp returns each trace once.
func TestGetTraceOverviews_LookBackBatchAtOneTimestamp(t *testing.T) {
	fake := lookBackFake(80, 1)
	for i := range fake.traces {
		fake.traces[i].StartTime = fake.traces[0].StartTime
		fake.traces[i].EndTime = fake.traces[0].StartTime
	}
	c := NewTracingController(fake)

	ids, _, truncated := traceIDs(c, t, lookBackParams(200))

	assertNoDuplicates(t, ids)
	if len(ids) != 80 || truncated {
		t.Errorf("got %d traces, truncated %v; want 80, false", len(ids), truncated)
	}
	if got := atomic.LoadInt32(&fake.queryTracesCalls); got != 2 {
		t.Errorf("QueryTraces calls = %d, want 2", got)
	}
}

// Traces that overlap a fetch boundary are kept, and traces whose root ends
// after the window, which still take up upstream limit slots, do not end the
// look-back early.
func TestGetTraceOverviews_LookBackOverlappingTraces(t *testing.T) {
	for _, sortOrder := range []string{"desc", "asc"} {
		t.Run(sortOrder, func(t *testing.T) {
			fake := lookBackFake(300, 1)
			for i := range fake.traces {
				fake.traces[i].EndTime = fake.traces[i].StartTime.Add(5 * time.Second)
			}
			c := NewTracingController(fake)
			params := lookBackParams(1000)
			params.SortOrder = sortOrder

			ids, lookedBackTo, truncated := traceIDs(c, t, params)

			assertNoDuplicates(t, ids)
			// Traces 0-3 end after the window, so their roots are out of it.
			if len(ids) != 296 || truncated {
				t.Fatalf("got %d traces, truncated %v; want 296, false", len(ids), truncated)
			}
			if want := formatCursor(windowEdge(params)); lookedBackTo != want {
				t.Errorf("lookedBackTo = %s, want %s", lookedBackTo, want)
			}
		})
	}
}

// Summary filters drop traces before enrichment but still count as examined.
func TestGetTraceOverviews_LookBackPreFilterCountsAsExamined(t *testing.T) {
	fake := lookBackFake(1000, 1)
	c := NewTracingController(fake)
	params := lookBackParams(10)
	params.Filters = TraceFilters{MinSpanCount: ptr(100)}

	ids, _, truncated := traceIDs(c, t, params)

	if len(ids) != 0 || !truncated {
		t.Fatalf("got %d traces, truncated %v; want 0, true", len(ids), truncated)
	}
	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != 0 {
		t.Errorf("GetSpanDetails calls = %d, want 0", got)
	}
}

// A cancelled context stops the loop between batches.
func TestGetTraceOverviews_LookBackHonoursCancel(t *testing.T) {
	fake := lookBackFake(400, 50)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.onQueryTraces = cancel
	c := NewTracingController(fake)

	_, err := c.GetTraceOverviews(ctx, lookBackParams(25))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&fake.queryTracesCalls); got != 1 {
		t.Errorf("QueryTraces calls = %d, want 1", got)
	}
}

// fakeClock is a controller clock that tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// now returns the fake time.
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// advance moves the fake time forward by d.
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// withClock gives c a fake clock that stands still until advanced.
func withClock(c *TracingController) *fakeClock {
	clock := &fakeClock{t: lookBackWindowEnd}
	c.now = clock.now
	return clock
}

// advanceOnRoot moves clock by d when rootID is fetched, during that trace's chunk.
func advanceOnRoot(fake *fakeObserverClient, clock *fakeClock, rootID string, d time.Duration) {
	var once sync.Once
	fake.onGetSpanDetails = func(spanID string) {
		if spanID == rootID {
			once.Do(func() { clock.advance(d) })
		}
	}
}

// logContext returns a context whose logger writes JSON lines to the buffer.
func logContext() (context.Context, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return logger.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(buf, nil))), buf
}

// logField returns field from the first log line with message msg.
func logField(t *testing.T, buf *bytes.Buffer, msg, field string) any {
	t.Helper()
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		var rec map[string]any
		if json.Unmarshal(line, &rec) == nil && rec["msg"] == msg {
			return rec[field]
		}
	}
	t.Fatalf("no %q log line", msg)
	return nil
}

// pageIDs lists the pages' trace IDs in order, leaving out the traces a page
// may repeat at the previous page's cursor time.
func pageIDs(t *testing.T, pages []cursorPage) []string {
	t.Helper()
	var ids []string
	var cur *TraceCursor
	for _, p := range pages {
		for _, tr := range p.resp.Traces {
			if cur == nil || tr.StartTime != formatCursor(cur.Time) {
				ids = append(ids, tr.TraceID)
			}
		}
		if p.resp.NextCursor != "" {
			var err error
			if cur, err = DecodeTraceCursor(p.resp.NextCursor); err != nil {
				t.Fatalf("nextCursor does not decode: %v", err)
			}
		}
	}
	return ids
}

// A sparse filter that runs out of time stops at a chunk boundary with a
// cursor, and paging on returns what a run with no budget returns.
func TestGetTraceOverviews_LookBackStopsAtTimeBudget(t *testing.T) {
	tests := []struct {
		name string
		// passOn is the root fetched when the clock passes the budget.
		passOn       string
		wantExamined int
		wantFetches  int32
	}{
		// Chunk 2 ends the second fetch, so the walk stops before the third.
		{name: "second chunk", passOn: "root-0050", wantExamined: 100, wantFetches: 2},
		// Chunk 3 starts the third fetch, so the walk stops before chunk 4.
		{name: "third chunk", passOn: "root-0100", wantExamined: 150, wantFetches: 3},
	}
	for _, matchEvery := range []int{1000, 45} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%s/1 in %d", tt.name, matchEvery), func(t *testing.T) {
				fake := lookBackFake(600, matchEvery)
				c := NewTracingController(fake)
				advanceOnRoot(fake, withClock(c), tt.passOn, listLookBackBudget)
				params := lookBackParams(10)
				ctx, logs := logContext()

				first, err := c.GetTraceOverviews(ctx, params)
				if err != nil {
					t.Fatalf("GetTraceOverviews returned error: %v", err)
				}

				if !first.Truncated || first.NextCursor == "" {
					t.Fatalf("truncated %v, nextCursor %q; want truncated with a cursor", first.Truncated, first.NextCursor)
				}
				if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != int32(tt.wantExamined) {
					t.Errorf("GetSpanDetails calls = %d, want %d (one root per examined trace)", got, tt.wantExamined)
				}
				if got := atomic.LoadInt32(&fake.queryTracesCalls); got != tt.wantFetches {
					t.Errorf("QueryTraces calls = %d, want %d", got, tt.wantFetches)
				}
				if want := formatCursor(fake.traces[tt.wantExamined-1].StartTime); first.LookedBackTo != want {
					t.Errorf("lookedBackTo = %s, want the last examined trace %s", first.LookedBackTo, want)
				}
				if got := logField(t, logs, "Retrieved trace overviews", "budgetExceeded"); got != true {
					t.Errorf("budgetExceeded logged as %v, want true", got)
				}

				if params.Cursor, err = DecodeTraceCursor(first.NextCursor); err != nil {
					t.Fatalf("nextCursor does not decode: %v", err)
				}
				got := pageIDs(t, append([]cursorPage{{resp: first}}, pageAll(t, c, fake, params)...))
				assertNoDuplicates(t, got)

				unbudgetedFake := lookBackFake(600, matchEvery)
				unbudgeted := NewTracingController(unbudgetedFake)
				withClock(unbudgeted)
				if want := pageIDs(t, pageAll(t, unbudgeted, unbudgetedFake, lookBackParams(10))); !reflect.DeepEqual(got, want) {
					t.Errorf("paged %v, want %v as with no budget", got, want)
				}
			})
		}
	}
}

// A budget already spent when the first fetch returns still examines one chunk.
func TestGetTraceOverviews_LookBackBudgetSpentOnFirstFetch(t *testing.T) {
	fake := lookBackFake(600, 1000)
	c := NewTracingController(fake)
	clock := withClock(c)
	fake.onQueryTraces = func() { clock.advance(listLookBackBudget) }

	resp, err := c.GetTraceOverviews(context.Background(), lookBackParams(10))
	if err != nil {
		t.Fatalf("GetTraceOverviews returned error: %v", err)
	}

	if len(resp.Traces) != 1 || resp.Traces[0].TraceID != "trace-0000" {
		t.Errorf("got %d traces, want trace-0000 alone", len(resp.Traces))
	}
	if !resp.Truncated || resp.NextCursor == "" {
		t.Fatalf("truncated %v, nextCursor %q; want truncated with a cursor", resp.Truncated, resp.NextCursor)
	}
	if got := atomic.LoadInt32(&fake.queryTracesCalls); got != 1 {
		t.Errorf("QueryTraces calls = %d, want 1", got)
	}
	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != lookBackBatchSize {
		t.Errorf("GetSpanDetails calls = %d, want %d (one chunk)", got, lookBackBatchSize)
	}
	if want := formatCursor(fake.traces[lookBackBatchSize-1].StartTime); resp.LookedBackTo != want {
		t.Errorf("lookedBackTo = %s, want %s", resp.LookedBackTo, want)
	}
}

// A clock short of the list's budget leaves the walk to the examine cap.
func TestGetTraceOverviews_LookBackWithinBudget(t *testing.T) {
	fake := lookBackFake(600, 1000)
	c := NewTracingController(fake)
	advanceOnRoot(fake, withClock(c), "root-0050", listLookBackBudget-time.Nanosecond)
	ctx, logs := logContext()

	resp, err := c.GetTraceOverviews(ctx, lookBackParams(10))
	if err != nil {
		t.Fatalf("GetTraceOverviews returned error: %v", err)
	}

	if got := atomic.LoadInt32(&fake.getSpanDetailsCalls); got != maxExaminedTraces {
		t.Errorf("GetSpanDetails calls = %d, want %d", got, maxExaminedTraces)
	}
	if !resp.Truncated || resp.NextCursor == "" {
		t.Errorf("truncated %v, nextCursor %q; want truncated with a cursor", resp.Truncated, resp.NextCursor)
	}
	if got := logField(t, logs, "Retrieved trace overviews", "budgetExceeded"); got != false {
		t.Errorf("budgetExceeded logged as %v, want false", got)
	}
}
