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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	deleteRoute        = "POST /internal/v1/scores/delete"
	backfillRoute      = "POST /internal/v1/scores/backfill"
	maxDeleteBodyBytes = 64 << 10
	maxDeleteMonitors  = 100
	// deleteTimeout stays under the server's write timeout so the answer still goes out.
	deleteTimeout = 25 * time.Second
)

// deleteRequest names the monitors whose scores go; an evaluator removal also names the evaluator and its cut-off.
type deleteRequest struct {
	OrgID            string     `json:"orgId"`
	MonitorIDs       []string   `json:"monitorIds"`
	EvaluatorName    *string    `json:"evaluatorName"`
	RunStartedBefore *time.Time `json:"runStartedBefore"`
}

// backfillRequest is one run's scores from agent-manager-service, which names whose they are.
type backfillRequest struct {
	OrgID            string      `json:"orgId"`
	MonitorID        string      `json:"monitorId"`
	RunID            string      `json:"runId"`
	RunStartedAt     *time.Time  `json:"runStartedAt"`
	ProjectName      string      `json:"projectName"`
	AgentName        string      `json:"agentName"`
	EnvironmentName  string      `json:"environmentName"`
	MonitorName      string      `json:"monitorName"`
	IndividualScores []scoreItem `json:"individualScores"`
}

// internalAudit is the one audit line each internal request logs; it never holds the token.
type internalAudit struct {
	start    time.Time
	action   string
	outcome  string
	status   int
	reason   string
	clientID string
	attrs    []any
}

func (a *internalAudit) log(remoteAddr string) {
	attrs := []any{
		"action", a.action,
		"outcome", a.outcome,
		"status", a.status,
		"remoteAddr", remoteAddr,
		"durationMs", time.Since(a.start).Milliseconds(),
	}
	if a.reason != "" {
		attrs = append(attrs, "reason", a.reason)
	}
	if a.clientID != "" {
		attrs = append(attrs, "clientId", a.clientID)
	}
	slog.Info("audit", append(attrs, a.attrs...)...)
}

// reject answers with a client error and records why.
func (a *internalAudit) reject(rw http.ResponseWriter, status int, reason, message string, fields []fieldError) {
	a.outcome, a.status, a.reason = "rejected", status, reason
	if status == http.StatusUnauthorized {
		rw.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	}
	writeError(rw, status, message, fields)
}

// fail answers 503 so the caller retries, and records why.
func (a *internalAudit) fail(rw http.ResponseWriter, reason, message string) {
	a.outcome, a.status, a.reason = "failed", http.StatusServiceUnavailable, reason
	writeError(rw, http.StatusServiceUnavailable, message, nil)
}

// authorizeService admits only agent-manager-service's client-credentials token.
func (w *Writer) authorizeService(rw http.ResponseWriter, r *http.Request, audit *internalAudit) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		audit.reject(rw, http.StatusUnauthorized, "missing bearer token", "agent-manager-service's client token is required", nil)
		return false
	}
	claims, err := w.serviceTokens.verify(r.Context(), token)
	switch {
	case errors.Is(err, errKeysUnavailable):
		slog.Error("Client-token keys unavailable", "error", err)
		audit.fail(rw, err.Error(), "client tokens can't be verified yet; retry")
		return false
	case errors.Is(err, errWrongClient):
		audit.clientID = truncate(claims.ClientID)
		audit.reject(rw, http.StatusForbidden, "client is not agent-manager-service", "this client may not call the internal routes", nil)
		return false
	case err != nil:
		audit.reject(rw, http.StatusUnauthorized, "invalid client token: "+err.Error(), "invalid or expired token", nil)
		return false
	}
	audit.clientID = claims.ClientID
	return true
}

// readBody reads at most limit bytes; past that it answers 413.
func readBody(rw http.ResponseWriter, r *http.Request, limit int64, audit *internalAudit) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(rw, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			audit.reject(rw, http.StatusRequestEntityTooLarge, "body too large", fmt.Sprintf("the body is larger than %d bytes", limit), nil)
			return nil, false
		}
		audit.reject(rw, http.StatusBadRequest, "body unreadable", "the body could not be read", nil)
		return nil, false
	}
	return body, true
}

// deleteScores deletes an org's scores for some monitors, or one evaluator's scores from runs started before a time.
func (w *Writer) deleteScores(rw http.ResponseWriter, r *http.Request) {
	audit := &internalAudit{start: time.Now(), action: "score.delete"}
	defer audit.log(r.RemoteAddr)
	if !w.authorizeService(rw, r, audit) {
		return
	}
	body, ok := readBody(rw, r, maxDeleteBodyBytes, audit)
	if !ok {
		return
	}
	req := &deleteRequest{}
	if fe := decodeStrict(body, req); fe != nil {
		audit.reject(rw, http.StatusBadRequest, "malformed body", "the body is not a valid delete request", []fieldError{*fe})
		return
	}
	if fields := checkDelete(req, w.now()); len(fields) > 0 {
		audit.reject(rw, http.StatusBadRequest, "invalid body", "the body is not a valid delete request", fields)
		return
	}
	audit.attrs = append(audit.attrs, "orgId", req.OrgID, "monitorIds", req.MonitorIDs, "monitorCount", len(req.MonitorIDs))
	if req.EvaluatorName != nil {
		audit.attrs = append(audit.attrs, "evaluatorName", *req.EvaluatorName, "runStartedBefore", req.RunStartedBefore.UTC())
	}

	ctx, cancel := context.WithTimeout(r.Context(), deleteTimeout)
	defer cancel()
	deleted, err := w.deleter.deleteScores(ctx, &deleteFilter{
		OrgID:            req.OrgID,
		MonitorIDs:       req.MonitorIDs,
		EvaluatorName:    req.EvaluatorName,
		RunStartedBefore: req.RunStartedBefore,
	})
	audit.attrs = append(audit.attrs, "traces", deleted)
	if err != nil {
		slog.Error("Failed to delete scores", "orgId", req.OrgID, "error", err)
		audit.fail(rw, "delete failed", "the score store is unavailable; retry")
		return
	}

	audit.outcome, audit.status = "deleted", http.StatusOK
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(rw).Encode(map[string]int64{"traceCount": deleted})
}

// checkDelete validates a delete; evaluatorName and runStartedBefore come together or not at all.
func checkDelete(req *deleteRequest, now time.Time) []fieldError {
	v := &validator{}
	checkName(v, "orgId", req.OrgID, maxRunClaimLength)
	switch n := len(req.MonitorIDs); {
	case n == 0:
		v.add("monitorIds", "must hold at least one monitor ID")
	case n > maxDeleteMonitors:
		v.add("monitorIds", "holds %d monitor IDs; at most %d per request", n, maxDeleteMonitors)
	}
	for i, id := range req.MonitorIDs {
		if !isUUID(id) {
			v.add(fmt.Sprintf("monitorIds[%d]", i), "must be a UUID")
		}
	}
	switch {
	case req.EvaluatorName != nil && req.RunStartedBefore == nil:
		v.add("runStartedBefore", "is required with evaluatorName")
	case req.EvaluatorName == nil && req.RunStartedBefore != nil:
		v.add("evaluatorName", "is required with runStartedBefore")
	case req.EvaluatorName != nil:
		checkName(v, "evaluatorName", *req.EvaluatorName, maxNameLength)
		if req.RunStartedBefore.After(now.Add(futureSkew)) {
			v.add("runStartedBefore", "is more than 5 minutes in the future")
		}
	}
	return v.errs
}

// backfillScores stores one run's scores with the identity agent-manager-service gives, as its system of record.
func (w *Writer) backfillScores(rw http.ResponseWriter, r *http.Request) {
	audit := &internalAudit{start: time.Now(), action: "score.backfill"}
	defer audit.log(r.RemoteAddr)
	if !w.authorizeService(rw, r, audit) {
		return
	}
	body, ok := readBody(rw, r, maxPublishBodyBytes, audit)
	if !ok {
		return
	}
	req := &backfillRequest{}
	if fe := decodeStrict(body, req); fe != nil {
		audit.reject(rw, http.StatusBadRequest, "malformed body", "the body is not a valid backfill request", []fieldError{*fe})
		return
	}
	traces, spans, fields := buildBackfillRows(req, w.now(), w.retention)
	if len(fields) > 0 {
		audit.reject(rw, http.StatusBadRequest, "invalid body", "the body is not a valid backfill request", fields)
		return
	}
	audit.attrs = append(audit.attrs, "orgId", req.OrgID, "monitorId", req.MonitorID, "runId", req.RunID)

	ctx, cancel := context.WithTimeout(r.Context(), publishWriteTimeout)
	defer cancel()
	err := w.store.publish(ctx, &publishBatch{
		OrgID:           req.OrgID,
		MonitorID:       req.MonitorID,
		RunID:           req.RunID,
		ProjectName:     req.ProjectName,
		AgentName:       req.AgentName,
		EnvironmentName: req.EnvironmentName,
		MonitorName:     req.MonitorName,
		RunStartedAt:    req.RunStartedAt.UTC(),
		Traces:          traces,
		Spans:           spans,
	})
	if err != nil {
		slog.Error("Failed to backfill scores", "monitorId", req.MonitorID, "runId", req.RunID, "error", err)
		audit.fail(rw, "write failed", "the score store is unavailable; retry")
		return
	}

	traceCount := countTraces(traces)
	audit.outcome, audit.status = "backfilled", http.StatusCreated
	audit.attrs = append(audit.attrs, "traces", traceCount, "spans", len(spans))
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(rw).Encode(map[string]int{"traceCount": traceCount, "spanCount": len(spans)})
}

// buildBackfillRows validates the identity in the body, then the scores as a publish does.
func buildBackfillRows(req *backfillRequest, now time.Time, retention time.Duration) ([]traceRow, []spanRow, []fieldError) {
	v := &validator{}
	for _, f := range []struct{ name, value string }{
		{"orgId", req.OrgID},
		{"projectName", req.ProjectName},
		{"agentName", req.AgentName},
		{"environmentName", req.EnvironmentName},
	} {
		checkName(v, f.name, f.value, maxRunClaimLength)
	}
	checkName(v, "monitorName", req.MonitorName, maxMonitorNameLen)
	if !isUUID(req.MonitorID) {
		v.add("monitorId", "must be a UUID")
	}
	if !isUUID(req.RunID) {
		v.add("runId", "must be a UUID")
	}
	switch {
	case req.RunStartedAt == nil:
		v.add("runStartedAt", "is required")
	case req.RunStartedAt.After(now.Add(futureSkew)):
		v.add("runStartedAt", "is more than 5 minutes in the future")
	}
	return v.rows(req.IndividualScores, now, retention)
}
