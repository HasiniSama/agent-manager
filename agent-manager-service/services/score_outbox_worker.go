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

package services

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/wso2/agent-manager/agent-manager-service/audit"
	"github.com/wso2/agent-manager/agent-manager-service/clients/scorewritersvc"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/repositories"
)

const (
	scoreOutboxTickInterval = 10 * time.Second
	// scoreOutboxBatchSize caps the rows one cycle sends.
	scoreOutboxBatchSize   = 50
	scoreOutboxBackoffBase = 10 * time.Second
	scoreOutboxBackoffCap  = 5 * time.Minute

	systemActorScoreOutbox = "system:score-outbox"
)

// ScoreOutboxWorker sends queued score deletes to the score writer.
type ScoreOutboxWorker interface {
	Start(ctx context.Context) error
	Stop() error
}

type scoreOutboxWorker struct {
	client   scorewritersvc.ScoreWriterClient
	tx       TxRunner
	repo     repositories.ScoreOutboxRepository
	logger   *slog.Logger
	now      func() time.Time
	running  bool
	stopCh   chan struct{}
	stopOnce sync.Once
}

// NewScoreOutboxWorker creates the worker; with a nil client it never starts.
func NewScoreOutboxWorker(
	client scorewritersvc.ScoreWriterClient,
	tx TxRunner,
	repo repositories.ScoreOutboxRepository,
	logger *slog.Logger,
) ScoreOutboxWorker {
	return &scoreOutboxWorker{
		client: client,
		tx:     tx,
		repo:   repo,
		logger: logger,
		now:    time.Now,
		stopCh: make(chan struct{}),
	}
}

// Start begins sending, unless no score writer is configured.
func (w *scoreOutboxWorker) Start(ctx context.Context) error {
	if w.client == nil {
		w.logger.Info("Score writer URL not set; score outbox worker not started")
		return nil
	}
	w.running = true
	go w.runLoop(ctx)
	w.logger.Info("Score outbox worker started")
	return nil
}

// Stop stops the worker.
func (w *scoreOutboxWorker) Stop() error {
	w.stopOnce.Do(func() {
		close(w.stopCh)
	})
	return nil
}

func (w *scoreOutboxWorker) runLoop(ctx context.Context) {
	ticker := time.NewTicker(scoreOutboxTickInterval)
	defer ticker.Stop()

	w.runCycle(ctx)
	for {
		select {
		case <-ticker.C:
			w.runCycle(ctx)
		case <-w.stopCh:
			w.logger.Info("Score outbox worker stopped")
			return
		case <-ctx.Done():
			return
		}
	}
}

// runCycle sends due rows while holding the advisory lock, so one replica sends at a time.
func (w *scoreOutboxWorker) runCycle(ctx context.Context) {
	err := w.tx(ctx, func(tx *gorm.DB) error {
		locked, err := w.repo.WithTx(tx).TryLock(ctx)
		if err != nil {
			return fmt.Errorf("failed to take the score outbox lock: %w", err)
		}
		if locked {
			w.sendDue(ctx)
		}
		return nil
	})
	if err != nil {
		w.logger.Error("Score outbox cycle failed", "error", err)
	}
}

// sendDue sends due rows oldest first, stopping at the first failure the writer may recover from.
func (w *scoreOutboxWorker) sendDue(ctx context.Context) {
	rows, err := w.repo.ListDue(ctx, w.now(), scoreOutboxBatchSize)
	if err != nil {
		w.logger.Error("Failed to list due score deletes", "error", err)
		return
	}
	for i := range rows {
		if err := w.send(ctx, &rows[i]); err != nil && !scorewritersvc.IsRejected(err) {
			break
		}
	}
	w.logBacklog(ctx)
}

// send posts one row and records the outcome on it; it returns the writer's error.
func (w *scoreOutboxWorker) send(ctx context.Context, row *models.ScoreOutboxRow) error {
	resend := row.SentAt != nil
	req := scoreDeleteRequest(row)
	sendErr := w.client.DeleteScores(ctx, req)
	w.recordSend(ctx, row, req, resend, sendErr)

	var markErr error
	switch {
	case sendErr == nil && resend:
		markErr = w.repo.MarkResent(ctx, row.ID)
	case sendErr == nil:
		markErr = w.repo.MarkSent(ctx, row.ID, w.now())
	case scorewritersvc.IsRejected(sendErr):
		w.logger.Error("Score writer rejected a score delete; not retrying",
			"kind", row.Kind, "id", row.ID, "error", sendErr)
		markErr = w.repo.MarkRejected(ctx, row.ID)
	default:
		retryAt := w.now().Add(scoreOutboxBackoff(row.Attempts + 1))
		w.logger.Warn("Failed to send a score delete",
			"kind", row.Kind, "id", row.ID, "retryAt", retryAt, "error", sendErr)
		markErr = w.repo.MarkFailed(ctx, row.ID, resend, retryAt)
	}
	if markErr != nil {
		w.logger.Error("Failed to record a score delete send", "kind", row.Kind, "id", row.ID, "error", markErr)
	}
	return sendErr
}

// recordSend audits one send; a record that cannot be written does not stop it.
func (w *scoreOutboxWorker) recordSend(
	ctx context.Context, row *models.ScoreOutboxRow, req scorewritersvc.DeleteScoresRequest, resend bool, err error,
) {
	action := audit.ActionMonitorScoreDelete
	opts := []audit.Option{
		audit.Org(req.OrgID),
		audit.Resource("score-outbox", strconv.FormatInt(row.ID, 10)),
		audit.Actor(audit.ActorSystem, systemActorScoreOutbox, ""),
		audit.SurfaceOpt(audit.SurfaceSystem),
		audit.Detail("monitorIds", req.MonitorIDs),
		audit.Detail("resend", resend),
		audit.Detail("attempt", row.Attempts+1),
		audit.Result(err),
	}
	if row.Kind == models.ScoreOutboxEvaluatorRemoved {
		action = audit.ActionMonitorScoreDeleteEvaluator
		opts = append(opts, audit.Detail("evaluatorName", req.EvaluatorName))
		if before := req.RunStartedBefore; before != nil {
			opts = append(opts, audit.Detail("runStartedBefore", before.UTC().Format(time.RFC3339Nano)))
		}
	}
	audit.Record(ctx, action, opts...)
}

// logBacklog logs the outbox depth and the age of the oldest unsent row.
func (w *scoreOutboxWorker) logBacklog(ctx context.Context) {
	backlog, err := w.repo.Backlog(ctx)
	if err != nil {
		w.logger.Error("Failed to read the score outbox backlog", "error", err)
		return
	}
	if backlog.Depth == 0 || backlog.OldestUnsentAt == nil {
		return
	}
	w.logger.Info("Score outbox backlog",
		"depth", backlog.Depth,
		"oldestUnsentAgeSeconds", int64(w.now().Sub(*backlog.OldestUnsentAt).Seconds()))
}

func scoreDeleteRequest(row *models.ScoreOutboxRow) scorewritersvc.DeleteScoresRequest {
	monitorIDs := make([]string, len(row.Payload.MonitorIDs))
	for i, id := range row.Payload.MonitorIDs {
		monitorIDs[i] = id.String()
	}
	return scorewritersvc.DeleteScoresRequest{
		OrgID:            row.Payload.OrgID,
		MonitorIDs:       monitorIDs,
		EvaluatorName:    row.Payload.EvaluatorName,
		RunStartedBefore: row.Payload.RunStartedBefore,
	}
}

// scoreOutboxBackoff doubles the base delay for each failed attempt, up to the cap.
func scoreOutboxBackoff(attempt int) time.Duration {
	delay := scoreOutboxBackoffBase
	for i := 1; i < attempt && delay < scoreOutboxBackoffCap; i++ {
		delay *= 2
	}
	return min(delay, scoreOutboxBackoffCap)
}
