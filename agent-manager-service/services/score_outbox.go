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
	"slices"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/wso2/agent-manager/agent-manager-service/config"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/repositories"
)

const (
	// scoreOutboxMaxMonitorIDs caps the monitor IDs in one row.
	scoreOutboxMaxMonitorIDs = 100
	// scoreOutboxResendMargin is added to the run deadline before the one resend.
	scoreOutboxResendMargin = 10 * time.Minute
)

// ScoreOutbox queues score deletes for the score writer in the transaction that makes the change.
type ScoreOutbox struct {
	tx   TxRunner
	repo repositories.ScoreOutboxRepository
	// resendDelay is the run deadline plus the margin, by which every run token minted before a change has expired.
	resendDelay time.Duration
	now         func() time.Time
}

// NewScoreOutbox creates a ScoreOutbox.
func NewScoreOutbox(tx TxRunner, repo repositories.ScoreOutboxRepository, cfg config.Config) *ScoreOutbox {
	return &ScoreOutbox{
		tx:          tx,
		repo:        repo,
		resendDelay: time.Duration(cfg.EvalRunToken.ActiveDeadlineSeconds)*time.Second + scoreOutboxResendMargin,
		now:         time.Now,
	}
}

// queueMonitorDeletes writes monitor_delete rows for monitorIDs in tx, at most 100 IDs per row.
func (o *ScoreOutbox) queueMonitorDeletes(ctx context.Context, tx *gorm.DB, orgID string, monitorIDs []uuid.UUID) error {
	now := o.now()
	var rows []models.ScoreOutboxRow
	for ids := range slices.Chunk(monitorIDs, scoreOutboxMaxMonitorIDs) {
		rows = append(rows, o.row(now, models.ScoreOutboxMonitorDelete, models.ScoreOutboxPayload{
			OrgID:      orgID,
			MonitorIDs: ids,
		}))
	}
	return o.insert(ctx, tx, rows)
}

// queueEvaluatorRemovals writes one evaluator_removed row per name in tx, for runs started before now.
func (o *ScoreOutbox) queueEvaluatorRemovals(ctx context.Context, tx *gorm.DB, orgID string, monitorID uuid.UUID, names []string) error {
	now := o.now()
	rows := make([]models.ScoreOutboxRow, 0, len(names))
	for _, name := range names {
		rows = append(rows, o.row(now, models.ScoreOutboxEvaluatorRemoved, models.ScoreOutboxPayload{
			OrgID:            orgID,
			MonitorIDs:       []uuid.UUID{monitorID},
			EvaluatorName:    name,
			RunStartedBefore: &now,
		}))
	}
	return o.insert(ctx, tx, rows)
}

// queueProjectDeletes queues score deletes for every monitor recorded under the project.
func (o *ScoreOutbox) queueProjectDeletes(ctx context.Context, orgID, projectName string) error {
	return o.tx(ctx, func(tx *gorm.DB) error {
		ids, err := o.repo.WithTx(tx).ProjectMonitorIDs(ctx, orgID, projectName)
		if err != nil {
			return fmt.Errorf("failed to list the project's monitors: %w", err)
		}
		return o.queueMonitorDeletes(ctx, tx, orgID, ids)
	})
}

func (o *ScoreOutbox) row(now time.Time, kind models.ScoreOutboxKind, payload models.ScoreOutboxPayload) models.ScoreOutboxRow {
	resendAfter := now.Add(o.resendDelay)
	return models.ScoreOutboxRow{
		Kind:          kind,
		Payload:       payload,
		CreatedAt:     now,
		NextAttemptAt: &now,
		ResendAfter:   &resendAfter,
	}
}

func (o *ScoreOutbox) insert(ctx context.Context, tx *gorm.DB, rows []models.ScoreOutboxRow) error {
	if len(rows) == 0 {
		return nil
	}
	if err := o.repo.WithTx(tx).Insert(ctx, rows); err != nil {
		return fmt.Errorf("failed to queue score deletes: %w", err)
	}
	return nil
}
