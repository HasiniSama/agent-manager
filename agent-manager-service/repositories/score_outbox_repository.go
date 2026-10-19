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

package repositories

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/wso2/agent-manager/agent-manager-service/models"
)

// scoreOutboxLockID is the advisory lock that keeps the outbox worker to one replica.
const scoreOutboxLockID = int64(739281458)

// ScoreOutboxRepository stores the score deletes queued for the score writer.
//
//go:generate moq -rm -fmt goimports -skip-ensure -pkg repomocks -out repomocks/score_outbox_repository_mock.go . ScoreOutboxRepository:ScoreOutboxRepositoryMock
type ScoreOutboxRepository interface {
	// WithTx returns the repository bound to tx, so its writes commit or roll back with the caller's.
	WithTx(tx *gorm.DB) ScoreOutboxRepository

	// Insert adds rows to the outbox.
	Insert(ctx context.Context, rows []models.ScoreOutboxRow) error
	// ProjectMonitorIDs lists the ID of every monitor recorded under the project.
	ProjectMonitorIDs(ctx context.Context, ouID, projectName string) ([]uuid.UUID, error)

	// TryLock takes the worker's lock until the transaction ends; false when another replica holds it.
	TryLock(ctx context.Context) (bool, error)
	// ListDue returns up to limit rows whose first send or resend is due, oldest first.
	ListDue(ctx context.Context, now time.Time, limit int) ([]models.ScoreOutboxRow, error)
	// MarkSent records the first successful send.
	MarkSent(ctx context.Context, id int64, sentAt time.Time) error
	// MarkResent records the successful resend, after which the row is done.
	MarkResent(ctx context.Context, id int64) error
	// MarkFailed counts a failed send and schedules the next try of the first send or the resend.
	MarkFailed(ctx context.Context, id int64, resend bool, retryAt time.Time) error
	// MarkRejected counts a send the writer refused and stops sending the row.
	MarkRejected(ctx context.Context, id int64) error
	// Backlog reports the rows still waiting for their first send.
	Backlog(ctx context.Context) (models.ScoreOutboxBacklog, error)
}

type scoreOutboxRepository struct {
	db *gorm.DB
}

// NewScoreOutboxRepository creates a ScoreOutboxRepository.
func NewScoreOutboxRepository(db *gorm.DB) ScoreOutboxRepository {
	return &scoreOutboxRepository{db: db}
}

func (r *scoreOutboxRepository) WithTx(tx *gorm.DB) ScoreOutboxRepository {
	return &scoreOutboxRepository{db: tx}
}

func (r *scoreOutboxRepository) Insert(ctx context.Context, rows []models.ScoreOutboxRow) error {
	if len(rows) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Create(&rows).Error
}

func (r *scoreOutboxRepository) ProjectMonitorIDs(ctx context.Context, ouID, projectName string) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := r.db.WithContext(ctx).Model(&models.Monitor{}).
		Where("ou_id = ? AND project_name = ?", ouID, projectName).
		Order("id").
		Pluck("id", &ids).Error
	return ids, err
}

func (r *scoreOutboxRepository) TryLock(ctx context.Context) (bool, error) {
	var locked bool
	err := r.db.WithContext(ctx).Raw("SELECT pg_try_advisory_xact_lock(?)", scoreOutboxLockID).Scan(&locked).Error
	return locked, err
}

func (r *scoreOutboxRepository) ListDue(ctx context.Context, now time.Time, limit int) ([]models.ScoreOutboxRow, error) {
	var rows []models.ScoreOutboxRow
	err := r.db.WithContext(ctx).
		Where("(sent_at IS NULL AND next_attempt_at <= ?) OR (sent_at IS NOT NULL AND resend_after <= ?)", now, now).
		Order("id").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}

func (r *scoreOutboxRepository) MarkSent(ctx context.Context, id int64, sentAt time.Time) error {
	return r.update(ctx, id, map[string]any{"sent_at": sentAt, "next_attempt_at": nil})
}

func (r *scoreOutboxRepository) MarkResent(ctx context.Context, id int64) error {
	return r.update(ctx, id, map[string]any{"resend_after": nil})
}

func (r *scoreOutboxRepository) MarkFailed(ctx context.Context, id int64, resend bool, retryAt time.Time) error {
	column := "next_attempt_at"
	if resend {
		column = "resend_after"
	}
	return r.update(ctx, id, map[string]any{"attempts": gorm.Expr("attempts + 1"), column: retryAt})
}

func (r *scoreOutboxRepository) MarkRejected(ctx context.Context, id int64) error {
	return r.update(ctx, id, map[string]any{
		"attempts":        gorm.Expr("attempts + 1"),
		"next_attempt_at": nil,
		"resend_after":    nil,
	})
}

func (r *scoreOutboxRepository) Backlog(ctx context.Context) (models.ScoreOutboxBacklog, error) {
	var backlog models.ScoreOutboxBacklog
	err := r.db.WithContext(ctx).Model(&models.ScoreOutboxRow{}).
		Select("COUNT(*) AS depth, MIN(created_at) AS oldest_unsent_at").
		Where("sent_at IS NULL AND next_attempt_at IS NOT NULL").
		Scan(&backlog).Error
	return backlog, err
}

func (r *scoreOutboxRepository) update(ctx context.Context, id int64, updates map[string]any) error {
	return r.db.WithContext(ctx).Model(&models.ScoreOutboxRow{}).Where("id = ?", id).Updates(updates).Error
}
