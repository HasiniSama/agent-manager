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
	"slices"
	"time"

	"github.com/wso2/agent-manager/agent-manager-observer/observer"
	"github.com/wso2/agent-manager/agent-manager-observer/opensearch"
)

// TraceStatusFilter selects traces by error status.
type TraceStatusFilter string

const (
	TraceStatusAny   TraceStatusFilter = ""
	TraceStatusError TraceStatusFilter = "error"
	TraceStatusOK    TraceStatusFilter = "ok"
)

// TraceFilters holds trace-list filters; set fields combine with AND.
// Min* are pointers so an explicit 0 still filters.
type TraceFilters struct {
	Status         TraceStatusFilter
	MinDurationMs  *int64
	MinTokens      *int64
	MinSpanCount   *int64
	Model          string
	ConversationID string
}

// IsZero reports whether no filter is set.
func (f TraceFilters) IsZero() bool {
	return f == TraceFilters{}
}

// matchesFilters reports whether overview satisfies f.
func matchesFilters(overview opensearch.TraceOverview, f TraceFilters) bool {
	hasErrors := overview.Status != nil && overview.Status.ErrorCount > 0
	switch f.Status {
	case TraceStatusError:
		if !hasErrors {
			return false
		}
	case TraceStatusOK:
		if hasErrors {
			return false
		}
	}
	if !matchesSummary(overview.DurationInNanos, overview.SpanCount, f) {
		return false
	}
	if f.MinTokens != nil && (overview.TokenUsage == nil || int64(overview.TokenUsage.TotalTokens) < *f.MinTokens) {
		return false
	}
	if f.Model != "" && !slices.Contains(overview.Models, f.Model) {
		return false
	}
	if f.ConversationID != "" && overview.ConversationID != f.ConversationID {
		return false
	}
	return true
}

// matchesSummary checks the filters the trace list alone can answer.
func matchesSummary(durationNs int64, spanCount int, f TraceFilters) bool {
	// Compare in ms to avoid overflow.
	if f.MinDurationMs != nil && durationNs/int64(time.Millisecond) < *f.MinDurationMs {
		return false
	}
	if f.MinSpanCount != nil && int64(spanCount) < *f.MinSpanCount {
		return false
	}
	return true
}

// filterTraceInfos drops traces the summary already rules out, before enrichment.
func filterTraceInfos(traces []observer.TraceInfo, f TraceFilters) []observer.TraceInfo {
	matched := make([]observer.TraceInfo, 0, len(traces))
	for _, t := range traces {
		if matchesSummary(t.DurationNs, t.SpanCount, f) {
			matched = append(matched, t)
		}
	}
	return matched
}

func filterOverviews(overviews []opensearch.TraceOverview, f TraceFilters) []opensearch.TraceOverview {
	matched := make([]opensearch.TraceOverview, 0, len(overviews))
	for _, ov := range overviews {
		if matchesFilters(ov, f) {
			matched = append(matched, ov)
		}
	}
	return matched
}
