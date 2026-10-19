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

package scorewriter

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxTracesPerRequest = 500
	maxNameLength       = 256
	maxMonitorNameLen   = 128
	maxFieldErrors      = 50
	futureSkew          = 5 * time.Minute
)

var (
	traceIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	spanIDPattern  = regexp.MustCompile(`^(?:[0-9a-f]{16}|[0-9a-f]{32})$`)
	uuidPattern    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// publishRequest is the publish body; who the scores belong to comes from the run token.
type publishRequest struct {
	Context          publishContext `json:"context"`
	IndividualScores []scoreItem    `json:"individualScores"`
}

type publishContext struct {
	MonitorName string `json:"monitorName"`
}

// scoreItem is one evaluator's result for a trace or one of its spans, as the evaluation job sends it.
type scoreItem struct {
	EvaluatorName       string       `json:"evaluatorName"`
	EvaluatorIdentifier string       `json:"evaluatorIdentifier"`
	Level               string       `json:"level"`
	TraceID             string       `json:"traceId"`
	TraceStartTime      *time.Time   `json:"traceStartTime"`
	Score               *float64     `json:"score"`
	SkipReason          *string      `json:"skipReason"`
	Explanation         *string      `json:"explanation"`
	SpanContext         *spanContext `json:"spanContext"`
}

type spanContext struct {
	SpanID    string  `json:"spanId"`
	AgentName *string `json:"agentName"`
	Model     *string `json:"model"`
	Vendor    *string `json:"vendor"`
}

// fieldError names the body field a validation failure is about.
type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// traceRow is one trace_scores row: one evaluator's result for one trace.
type traceRow struct {
	EvaluatorName       string
	EvaluatorIdentifier string
	Level               string
	TraceID             string
	TraceStartTime      time.Time
	Score               *float64
	ScoreSum            float64
	ScoredCount         int32
	SkippedCount        int32
	SkipReason          *string
	Explanation         *string
}

// spanRow is one span_scores row.
type spanRow struct {
	EvaluatorName string
	TraceID       string
	SpanID        string
	Label         string
	Score         *float64
	SkipReason    *string
	Explanation   *string
}

// validator collects field errors, up to maxFieldErrors.
type validator struct {
	errs []fieldError
}

func (v *validator) add(field, format string, args ...any) {
	if len(v.errs) < maxFieldErrors {
		v.errs = append(v.errs, fieldError{Field: field, Message: fmt.Sprintf(format, args...)})
	}
}

// buildRows validates the body and groups its items into one trace row per evaluator and trace.
func buildRows(req *publishRequest, now time.Time, retention time.Duration) ([]traceRow, []spanRow, []fieldError) {
	v := &validator{}
	checkName(v, "context.monitorName", req.Context.MonitorName, maxMonitorNameLen)
	if len(req.IndividualScores) == 0 {
		v.add("individualScores", "must hold at least one score")
	}

	oldest, newest := now.Add(-retention), now.Add(futureSkew)
	var (
		traces    []traceRow
		spans     []spanRow
		index     = map[[2]string]int{}
		spanSeen  = map[[3]string]bool{}
		traceSeen = map[[2]string]bool{}
		traceIDs  = map[string]bool{}
	)
	for i := range req.IndividualScores {
		item := &req.IndividualScores[i]
		path := fmt.Sprintf("individualScores[%d]", i)
		if !checkItem(v, path, item, oldest, newest) {
			continue
		}
		traceIDs[item.TraceID] = true
		identifier := item.EvaluatorIdentifier
		if identifier == "" {
			identifier = item.EvaluatorName
		}

		key := [2]string{item.EvaluatorName, item.TraceID}
		ti, ok := index[key]
		if !ok {
			ti = len(traces)
			index[key] = ti
			traces = append(traces, traceRow{
				EvaluatorName:       item.EvaluatorName,
				EvaluatorIdentifier: identifier,
				Level:               item.Level,
				TraceID:             item.TraceID,
				TraceStartTime:      item.TraceStartTime.UTC(),
			})
		}
		row := &traces[ti]
		switch {
		case row.Level != item.Level:
			v.add(path+".level", "differs from another item for this evaluator and trace")
			continue
		case row.EvaluatorIdentifier != identifier:
			v.add(path+".evaluatorIdentifier", "differs from another item for this evaluator and trace")
			continue
		case !row.TraceStartTime.Equal(*item.TraceStartTime):
			v.add(path+".traceStartTime", "differs from another item for this trace")
			continue
		}

		if item.SpanContext == nil {
			if traceSeen[key] {
				v.add(path, "is a second trace-level result for this evaluator and trace")
				continue
			}
			traceSeen[key] = true
			row.SkipReason = item.SkipReason
			row.Explanation = item.Explanation
		} else {
			spanKey := [3]string{item.EvaluatorName, item.TraceID, item.SpanContext.SpanID}
			if spanSeen[spanKey] {
				v.add(path+".spanContext.spanId", "is repeated for this evaluator and trace")
				continue
			}
			spanSeen[spanKey] = true
			spans = append(spans, spanRow{
				EvaluatorName: item.EvaluatorName,
				TraceID:       item.TraceID,
				SpanID:        item.SpanContext.SpanID,
				Label:         spanLabel(item.Level, item.SpanContext),
				Score:         item.Score,
				SkipReason:    item.SkipReason,
				Explanation:   item.Explanation,
			})
		}
		if item.Score != nil {
			row.ScoredCount++
			row.ScoreSum += *item.Score
		} else {
			row.SkippedCount++
		}
	}
	if len(traceIDs) > maxTracesPerRequest {
		v.add("individualScores", "covers %d traces; at most %d per request", len(traceIDs), maxTracesPerRequest)
	}
	if len(v.errs) > 0 {
		return nil, nil, v.errs
	}
	for i := range traces {
		if traces[i].ScoredCount > 0 {
			mean := traces[i].ScoreSum / float64(traces[i].ScoredCount)
			traces[i].Score = &mean
		}
	}
	return traces, spans, nil
}

// checkItem validates one item and reports whether it can be grouped.
func checkItem(v *validator, path string, item *scoreItem, oldest, newest time.Time) bool {
	before := len(v.errs)
	checkName(v, path+".evaluatorName", item.EvaluatorName, maxNameLength)
	if item.EvaluatorIdentifier != "" {
		checkName(v, path+".evaluatorIdentifier", item.EvaluatorIdentifier, maxNameLength)
	}
	switch item.Level {
	case "trace", "agent", "llm":
	default:
		v.add(path+".level", "must be trace, agent or llm")
	}
	if !traceIDPattern.MatchString(item.TraceID) {
		v.add(path+".traceId", "must be 32 lowercase hex characters")
	}
	switch {
	case item.TraceStartTime == nil:
		v.add(path+".traceStartTime", "is required")
	case item.TraceStartTime.Before(oldest):
		v.add(path+".traceStartTime", "is older than the retention window")
	case item.TraceStartTime.After(newest):
		v.add(path+".traceStartTime", "is more than 5 minutes in the future")
	}
	switch {
	case item.Score != nil && item.SkipReason != nil:
		v.add(path, "must have a score or a skipReason, not both")
	case item.Score == nil && item.SkipReason == nil:
		v.add(path, "must have a score or a skipReason")
	case item.Score != nil && (math.IsNaN(*item.Score) || *item.Score < 0 || *item.Score > 1):
		v.add(path+".score", "must be between 0 and 1")
	case item.SkipReason != nil:
		checkText(v, path+".skipReason", *item.SkipReason, true)
	}
	if item.Explanation != nil {
		checkText(v, path+".explanation", *item.Explanation, false)
	}
	if sc := item.SpanContext; sc != nil {
		if !spanIDPattern.MatchString(sc.SpanID) {
			v.add(path+".spanContext.spanId", "must be 16 or 32 lowercase hex characters")
		}
		for _, f := range []struct {
			name  string
			value *string
		}{{"agentName", sc.AgentName}, {"model", sc.Model}, {"vendor", sc.Vendor}} {
			if f.value != nil && *f.value != "" {
				checkName(v, path+".spanContext."+f.name, *f.value, maxNameLength)
			}
		}
	}
	return len(v.errs) == before
}

// checkName requires 1 to max characters of valid text without control characters.
func checkName(v *validator, field, value string, max int) {
	switch {
	case strings.TrimSpace(value) == "":
		v.add(field, "is required")
	case utf8.RuneCountInString(value) > max:
		v.add(field, "is longer than %d characters", max)
	case strings.IndexFunc(value, unicode.IsControl) >= 0 || !utf8.ValidString(value):
		v.add(field, "has control characters or invalid UTF-8")
	}
}

// checkText rejects what Postgres text can't store.
func checkText(v *validator, field, value string, required bool) {
	switch {
	case required && strings.TrimSpace(value) == "":
		v.add(field, "must not be empty")
	case strings.ContainsRune(value, 0) || !utf8.ValidString(value):
		v.add(field, "has a NUL character or invalid UTF-8")
	}
}

// spanLabel names the scored span as the dashboards show it: the agent, or vendor/model.
func spanLabel(level string, sc *spanContext) string {
	deref := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}
	switch level {
	case "agent":
		return deref(sc.AgentName)
	case "llm":
		model, vendor := deref(sc.Model), deref(sc.Vendor)
		if model != "" && vendor != "" {
			return vendor + "/" + model
		}
		return model
	}
	return ""
}

func isUUID(s string) bool {
	return uuidPattern.MatchString(s)
}
