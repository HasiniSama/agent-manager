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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	maxPublishBodyBytes = 8 << 20
	publishWriteTimeout = 20 * time.Second
	publishRoute        = "POST /api/v1/publisher/monitors/{monitorId}/runs/{runId}/scores"
)

// publishAudit is the one audit line each publish request logs; it never holds the token.
type publishAudit struct {
	start       time.Time
	outcome     string
	status      int
	reason      string
	pathMonitor string
	pathRun     string
	claims      *RunClaims
	traces      int
	spans       int
}

func (a *publishAudit) log(remoteAddr string) {
	attrs := []any{
		"action", "score.publish",
		"outcome", a.outcome,
		"status", a.status,
		"traces", a.traces,
		"spans", a.spans,
		"remoteAddr", remoteAddr,
		"durationMs", time.Since(a.start).Milliseconds(),
	}
	if a.reason != "" {
		attrs = append(attrs, "reason", a.reason)
	}
	if c := a.claims; c != nil {
		attrs = append(attrs, "orgId", c.OuID, "monitorId", c.MonitorID, "runId", c.RunID, "jti", c.ID)
	}
	if a.claims == nil || a.pathMonitor != a.claims.MonitorID || a.pathRun != a.claims.RunID {
		attrs = append(attrs, "pathMonitorId", truncate(a.pathMonitor), "pathRunId", truncate(a.pathRun))
	}
	slog.Info("audit", attrs...)
}

// publishScores stores one run's scores for the org, monitor and run its run token names.
func (w *Writer) publishScores(rw http.ResponseWriter, r *http.Request) {
	audit := &publishAudit{start: time.Now(), pathMonitor: r.PathValue("monitorId"), pathRun: r.PathValue("runId")}
	defer audit.log(r.RemoteAddr)
	reject := func(status int, reason, message string, fields []fieldError) {
		audit.outcome, audit.status, audit.reason = "rejected", status, reason
		if status == http.StatusUnauthorized {
			rw.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		}
		writeError(rw, status, message, fields)
	}

	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		reject(http.StatusUnauthorized, "missing bearer token", "a run token is required", nil)
		return
	}
	claims, err := w.runTokens.verify(r.Context(), token)
	if errors.Is(err, errKeysUnavailable) {
		slog.Error("Run-token keys unavailable", "error", err)
		audit.outcome, audit.status, audit.reason = "failed", http.StatusServiceUnavailable, err.Error()
		writeError(rw, http.StatusServiceUnavailable, "run tokens can't be verified yet; retry", nil)
		return
	}
	if err != nil {
		reject(http.StatusUnauthorized, "invalid run token: "+err.Error(), "invalid or expired run token", nil)
		return
	}
	audit.claims = claims
	if !strings.EqualFold(audit.pathMonitor, claims.MonitorID) || !strings.EqualFold(audit.pathRun, claims.RunID) {
		reject(http.StatusForbidden, "path monitor or run differs from the run token", "the run token does not cover this monitor and run", nil)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, maxPublishBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			reject(http.StatusRequestEntityTooLarge, "body over 8 MiB", "the body is larger than 8 MiB", nil)
			return
		}
		reject(http.StatusBadRequest, "body unreadable", "the body could not be read", nil)
		return
	}
	req, fe := decodePublishRequest(body)
	if fe != nil {
		reject(http.StatusBadRequest, "malformed body", "the body is not a valid publish request", []fieldError{*fe})
		return
	}
	traces, spans, fields := buildRows(req, w.now(), w.retention)
	if len(fields) > 0 {
		reject(http.StatusBadRequest, "invalid body", "the body is not a valid publish request", fields)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), publishWriteTimeout)
	defer cancel()
	err = w.store.publish(ctx, &publishBatch{
		OrgID:           claims.OuID,
		MonitorID:       claims.MonitorID,
		RunID:           claims.RunID,
		ProjectName:     claims.ProjectName,
		AgentName:       claims.AgentName,
		EnvironmentName: claims.EnvironmentName,
		MonitorName:     req.Context.MonitorName,
		RunStartedAt:    claims.IssuedAt.UTC(),
		Traces:          traces,
		Spans:           spans,
	})
	if err != nil {
		slog.Error("Failed to store scores", "monitorId", claims.MonitorID, "runId", claims.RunID, "error", err)
		audit.outcome, audit.status, audit.reason = "failed", http.StatusServiceUnavailable, "write failed"
		writeError(rw, http.StatusServiceUnavailable, "the score store is unavailable; retry", nil)
		return
	}

	audit.outcome, audit.status = "published", http.StatusCreated
	audit.traces, audit.spans = countTraces(traces), len(spans)
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(rw).Encode(map[string]int{"traceCount": audit.traces, "spanCount": audit.spans})
}

// decodePublishRequest parses the body strictly: unknown fields and trailing data are errors.
func decodePublishRequest(body []byte) (*publishRequest, *fieldError) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var req publishRequest
	if err := dec.Decode(&req); err != nil {
		return nil, jsonFieldError(err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, &fieldError{Field: "", Message: "has data after the JSON object"}
	}
	return &req, nil
}

// jsonFieldError names the field a decode error is about where the decoder says.
func jsonFieldError(err error) *fieldError {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return &fieldError{Field: typeErr.Field, Message: "must be a " + typeErr.Type.String()}
	}
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return &fieldError{Field: strings.Trim(name, `"`), Message: "is not a known field"}
	}
	return &fieldError{Field: "", Message: "is not valid JSON"}
}

func writeError(rw http.ResponseWriter, status int, message string, fields []fieldError) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(struct {
		Error   string       `json:"error"`
		Message string       `json:"message"`
		Fields  []fieldError `json:"fields,omitempty"`
	}{http.StatusText(status), message, fields})
}

func countTraces(rows []traceRow) int {
	seen := make(map[string]struct{}, len(rows))
	for _, r := range rows {
		seen[r.TraceID] = struct{}{}
	}
	return len(seen)
}

func truncate(s string) string {
	const max = 64
	if len(s) > max {
		return s[:max]
	}
	return s
}
