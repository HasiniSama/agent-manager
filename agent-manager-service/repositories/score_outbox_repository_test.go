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

//go:build integration

package repositories

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/wso2/agent-manager/agent-manager-service/db"
	"github.com/wso2/agent-manager/agent-manager-service/models"
)

// newOutboxRow returns a row created at createdAt for a fresh org, due at once and resendable after resendAfter.
func newOutboxRow(createdAt time.Time, resendAfter time.Duration) models.ScoreOutboxRow {
	resendAt := createdAt.Add(resendAfter)
	return models.ScoreOutboxRow{
		Kind:          models.ScoreOutboxMonitorDelete,
		Payload:       models.ScoreOutboxPayload{OrgID: "ou-" + uuid.NewString()[:8], MonitorIDs: []uuid.UUID{uuid.New()}},
		CreatedAt:     createdAt,
		NextAttemptAt: &createdAt,
		ResendAfter:   &resendAt,
	}
}

func insertOutboxRow(t *testing.T, repo ScoreOutboxRepository, row models.ScoreOutboxRow) int64 {
	t.Helper()
	rows := []models.ScoreOutboxRow{row}
	require.NoError(t, repo.Insert(context.Background(), rows))
	t.Cleanup(func() { db.GetDB().Delete(&models.ScoreOutboxRow{}, rows[0].ID) })
	return rows[0].ID
}

// dueIDs lists the due rows at now among ids.
func dueIDs(t *testing.T, repo ScoreOutboxRepository, now time.Time, ids ...int64) []int64 {
	t.Helper()
	rows, err := repo.ListDue(context.Background(), now, 1000)
	require.NoError(t, err)
	var due []int64
	for _, row := range rows {
		for _, id := range ids {
			if row.ID == id {
				due = append(due, id)
			}
		}
	}
	return due
}

func TestScoreOutboxRowGetsOneSendAndOneResend(t *testing.T) {
	repo := NewScoreOutboxRepository(db.GetDB())
	ctx := context.Background()
	created := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	row := newOutboxRow(created, time.Hour)
	id := insertOutboxRow(t, repo, row)

	var stored models.ScoreOutboxRow
	require.NoError(t, db.GetDB().First(&stored, id).Error)
	assert.Equal(t, row.Payload, stored.Payload)

	assert.Equal(t, []int64{id}, dueIDs(t, repo, created, id), "a new row is due at once")
	require.NoError(t, repo.MarkSent(ctx, id, created))
	assert.Empty(t, dueIDs(t, repo, created.Add(59*time.Minute), id), "a sent row waits for its resend")

	assert.Equal(t, []int64{id}, dueIDs(t, repo, created.Add(time.Hour), id), "the resend is due after resend_after")
	require.NoError(t, repo.MarkResent(ctx, id))
	assert.Empty(t, dueIDs(t, repo, created.Add(24*time.Hour), id), "the row is done after its resend")
}

func TestScoreOutboxFailedSendsBackOffAndRejectedRowsStop(t *testing.T) {
	repo := NewScoreOutboxRepository(db.GetDB())
	ctx := context.Background()
	created := time.Now().Add(-time.Minute).UTC().Truncate(time.Microsecond)
	failed := insertOutboxRow(t, repo, newOutboxRow(created, time.Hour))
	rejected := insertOutboxRow(t, repo, newOutboxRow(created, time.Hour))

	require.NoError(t, repo.MarkFailed(ctx, failed, false, created.Add(10*time.Second)))
	require.NoError(t, repo.MarkRejected(ctx, rejected))

	assert.Empty(t, dueIDs(t, repo, created.Add(9*time.Second), failed, rejected))
	assert.Equal(t, []int64{failed}, dueIDs(t, repo, created.Add(10*time.Second), failed, rejected))
	assert.Empty(t, dueIDs(t, repo, created.Add(24*time.Hour), rejected), "a rejected row is never sent again")

	var stored models.ScoreOutboxRow
	require.NoError(t, db.GetDB().First(&stored, failed).Error)
	assert.Equal(t, 1, stored.Attempts)
}

func TestScoreOutboxInsertRollsBackWithItsTransaction(t *testing.T) {
	repo := NewScoreOutboxRepository(db.GetDB())
	row := newOutboxRow(time.Now().UTC(), time.Hour)
	rollback := errors.New("roll back")

	err := db.GetDB().Transaction(func(tx *gorm.DB) error {
		require.NoError(t, repo.WithTx(tx).Insert(context.Background(), []models.ScoreOutboxRow{row}))
		return rollback
	})

	require.ErrorIs(t, err, rollback)
	var count int64
	require.NoError(t, db.GetDB().Model(&models.ScoreOutboxRow{}).
		Where("payload->>'orgId' = ?", row.Payload.OrgID).Count(&count).Error)
	assert.Zero(t, count)
}

func TestScoreOutboxProjectMonitorIDsStaysInTheProject(t *testing.T) {
	monitors := NewMonitorRepo(db.GetDB())
	ouID := "ou-" + uuid.NewString()[:8]
	create := func(projectName, name string) uuid.UUID {
		m := &models.Monitor{
			Name: name, Type: models.MonitorTypeFuture, OUID: ouID, ProjectName: projectName,
			AgentName: "agent", AgentID: "agent-id", EnvironmentName: "dev", EnvironmentID: "env-id", SamplingRate: 1,
		}
		require.NoError(t, monitors.CreateMonitor(m))
		t.Cleanup(func() { _ = monitors.DeleteMonitor(m) })
		return m.ID
	}
	inProject := []uuid.UUID{create("checkout", "first"), create("checkout", "second")}
	create("billing", "third")

	ids, err := NewScoreOutboxRepository(db.GetDB()).ProjectMonitorIDs(context.Background(), ouID, "checkout")

	require.NoError(t, err)
	assert.ElementsMatch(t, inProject, ids)
}

func TestScoreOutboxLockAdmitsOneTransaction(t *testing.T) {
	repo := NewScoreOutboxRepository(db.GetDB())
	ctx := context.Background()
	holder := db.GetDB().Begin()
	defer holder.Rollback()

	locked, err := repo.WithTx(holder).TryLock(ctx)
	require.NoError(t, err)
	require.True(t, locked)

	err = db.GetDB().Transaction(func(tx *gorm.DB) error {
		other, err := repo.WithTx(tx).TryLock(ctx)
		require.NoError(t, err)
		assert.False(t, other, "a second transaction cannot take the lock")
		return nil
	})
	require.NoError(t, err)
}
