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
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/wso2/agent-manager/agent-manager-service/clients/clientmocks"
	"github.com/wso2/agent-manager/agent-manager-service/clients/requests"
	"github.com/wso2/agent-manager/agent-manager-service/clients/scorewritersvc"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/repositories"
	"github.com/wso2/agent-manager/agent-manager-service/repositories/repomocks"
)

// advisoryLock is a transaction-scoped lock: the transaction that takes it releases it when it ends.
type advisoryLock struct {
	owner *gorm.DB
}

func (l *advisoryLock) run(_ context.Context, fn func(tx *gorm.DB) error) error {
	tx := &gorm.DB{}
	defer func() {
		if l.owner == tx {
			l.owner = nil
		}
	}()
	return fn(tx)
}

func (l *advisoryLock) tryLock(tx *gorm.DB) bool {
	if l.owner != nil {
		return false
	}
	l.owner = tx
	return true
}

// lockedRepo returns an outbox repository whose transaction-bound copy takes lock.
func lockedRepo(lock *advisoryLock) *repomocks.ScoreOutboxRepositoryMock {
	return &repomocks.ScoreOutboxRepositoryMock{
		WithTxFunc: func(tx *gorm.DB) repositories.ScoreOutboxRepository {
			return &repomocks.ScoreOutboxRepositoryMock{
				TryLockFunc: func(context.Context) (bool, error) { return lock.tryLock(tx), nil },
			}
		},
		BacklogFunc: func(context.Context) (models.ScoreOutboxBacklog, error) {
			return models.ScoreOutboxBacklog{}, nil
		},
	}
}

func newTestOutboxWorker(client scorewritersvc.ScoreWriterClient, tx TxRunner, repo repositories.ScoreOutboxRepository) *scoreOutboxWorker {
	w := NewScoreOutboxWorker(client, tx, repo, discardLogger()).(*scoreOutboxWorker)
	w.now = func() time.Time { return outboxNow }
	return w
}

func dueRows(rows ...models.ScoreOutboxRow) func(context.Context, time.Time, int) ([]models.ScoreOutboxRow, error) {
	return func(context.Context, time.Time, int) ([]models.ScoreOutboxRow, error) {
		return rows, nil
	}
}

func writerStatus(code int) error {
	return &requests.HttpError{StatusCode: code}
}

func TestScoreOutboxWorker_DoesNotStartWithoutWriterURL(t *testing.T) {
	cycles := 0
	tx := func(context.Context, func(*gorm.DB) error) error {
		cycles++
		return nil
	}
	w := newTestOutboxWorker(nil, tx, &repomocks.ScoreOutboxRepositoryMock{})

	require.NoError(t, w.Start(auditableCtx(t)))
	require.NoError(t, w.Stop())

	assert.False(t, w.running)
	assert.Zero(t, cycles)
}

func TestScoreOutboxWorker_StartsWithWriterURL(t *testing.T) {
	ran := make(chan struct{}, 1)
	tx := func(context.Context, func(*gorm.DB) error) error {
		select {
		case ran <- struct{}{}:
		default:
		}
		return nil
	}
	w := newTestOutboxWorker(&clientmocks.ScoreWriterClientMock{}, tx, &repomocks.ScoreOutboxRepositoryMock{})

	require.NoError(t, w.Start(auditableCtx(t)))
	defer func() { require.NoError(t, w.Stop()) }()

	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not run a cycle")
	}
}

func TestScoreOutboxWorker_SendOutcomes(t *testing.T) {
	sentAt := outboxNow.Add(-time.Hour)
	for _, tc := range []struct {
		name       string
		row        models.ScoreOutboxRow
		sendErr    error
		wantMark   string
		wantResend bool
		wantRetry  time.Duration
	}{
		{name: "2xx marks the first send", row: models.ScoreOutboxRow{ID: 7}, wantMark: "sent"},
		{name: "2xx on a sent row marks the resend", row: models.ScoreOutboxRow{ID: 7, SentAt: &sentAt}, wantMark: "resent"},
		{name: "5xx backs off", row: models.ScoreOutboxRow{ID: 7}, sendErr: writerStatus(http.StatusServiceUnavailable), wantMark: "failed", wantRetry: 10 * time.Second},
		{name: "5xx backs off longer each attempt", row: models.ScoreOutboxRow{ID: 7, Attempts: 3}, sendErr: writerStatus(http.StatusBadGateway), wantMark: "failed", wantRetry: 80 * time.Second},
		{name: "backoff is capped at 5 minutes", row: models.ScoreOutboxRow{ID: 7, Attempts: 20}, sendErr: writerStatus(http.StatusInternalServerError), wantMark: "failed", wantRetry: 5 * time.Minute},
		{name: "a failed resend backs off the resend", row: models.ScoreOutboxRow{ID: 7, SentAt: &sentAt}, sendErr: writerStatus(http.StatusServiceUnavailable), wantMark: "failed", wantResend: true, wantRetry: 10 * time.Second},
		{name: "an unreachable writer backs off", row: models.ScoreOutboxRow{ID: 7}, sendErr: errors.New("connection refused"), wantMark: "failed", wantRetry: 10 * time.Second},
		{name: "401 is retried", row: models.ScoreOutboxRow{ID: 7}, sendErr: writerStatus(http.StatusUnauthorized), wantMark: "failed", wantRetry: 10 * time.Second},
		{name: "4xx stops retrying the row", row: models.ScoreOutboxRow{ID: 7}, sendErr: writerStatus(http.StatusBadRequest), wantMark: "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var marked string
			lock := &advisoryLock{}
			repo := lockedRepo(lock)
			repo.ListDueFunc = dueRows(tc.row)
			// Only the expected outcome is recorded; the other marks are nil and panic.
			switch tc.wantMark {
			case "sent":
				repo.MarkSentFunc = func(_ context.Context, id int64, at time.Time) error {
					assert.Equal(t, int64(7), id)
					assert.Equal(t, outboxNow, at)
					marked = "sent"
					return nil
				}
			case "resent":
				repo.MarkResentFunc = func(_ context.Context, id int64) error {
					assert.Equal(t, int64(7), id)
					marked = "resent"
					return nil
				}
			case "failed":
				repo.MarkFailedFunc = func(_ context.Context, id int64, resend bool, retryAt time.Time) error {
					assert.Equal(t, int64(7), id)
					assert.Equal(t, tc.wantResend, resend)
					assert.Equal(t, outboxNow.Add(tc.wantRetry), retryAt)
					marked = "failed"
					return nil
				}
			case "rejected":
				repo.MarkRejectedFunc = func(_ context.Context, id int64) error {
					assert.Equal(t, int64(7), id)
					marked = "rejected"
					return nil
				}
			}
			client := &clientmocks.ScoreWriterClientMock{
				DeleteScoresFunc: func(context.Context, scorewritersvc.DeleteScoresRequest) error { return tc.sendErr },
			}

			newTestOutboxWorker(client, lock.run, repo).runCycle(auditableCtx(t))

			assert.Equal(t, tc.wantMark, marked)
		})
	}
}

func TestScoreOutboxWorker_SendsTheRowsDelete(t *testing.T) {
	monitorID := uuid.New()
	before := outboxNow.Add(-time.Minute)
	lock := &advisoryLock{}
	repo := lockedRepo(lock)
	repo.ListDueFunc = dueRows(models.ScoreOutboxRow{
		ID:   1,
		Kind: models.ScoreOutboxEvaluatorRemoved,
		Payload: models.ScoreOutboxPayload{
			OrgID: "org-1", MonitorIDs: []uuid.UUID{monitorID}, EvaluatorName: "tone", RunStartedBefore: &before,
		},
	})
	repo.MarkSentFunc = func(context.Context, int64, time.Time) error { return nil }
	client := &clientmocks.ScoreWriterClientMock{
		DeleteScoresFunc: func(context.Context, scorewritersvc.DeleteScoresRequest) error { return nil },
	}

	newTestOutboxWorker(client, lock.run, repo).runCycle(auditableCtx(t))

	require.Len(t, client.DeleteScoresCalls(), 1)
	assert.Equal(t, scorewritersvc.DeleteScoresRequest{
		OrgID: "org-1", MonitorIDs: []string{monitorID.String()}, EvaluatorName: "tone", RunStartedBefore: &before,
	}, client.DeleteScoresCalls()[0].Req)
}

func TestScoreOutboxWorker_SendsOldestFirstAndStopsAtARetryableFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		firstErr  error
		wantSends []int64
	}{
		{name: "a 5xx leaves later rows for the next cycle", firstErr: writerStatus(http.StatusServiceUnavailable), wantSends: []int64{1}},
		{name: "a 4xx moves on to the next row", firstErr: writerStatus(http.StatusBadRequest), wantSends: []int64{1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lock := &advisoryLock{}
			repo := lockedRepo(lock)
			repo.ListDueFunc = func(_ context.Context, now time.Time, limit int) ([]models.ScoreOutboxRow, error) {
				assert.Equal(t, outboxNow, now)
				assert.Equal(t, scoreOutboxBatchSize, limit)
				return []models.ScoreOutboxRow{{ID: 1}, {ID: 2}}, nil
			}
			repo.MarkFailedFunc = func(context.Context, int64, bool, time.Time) error { return nil }
			repo.MarkRejectedFunc = func(context.Context, int64) error { return nil }
			repo.MarkSentFunc = func(context.Context, int64, time.Time) error { return nil }
			var sends []int64
			client := &clientmocks.ScoreWriterClientMock{
				DeleteScoresFunc: func(context.Context, scorewritersvc.DeleteScoresRequest) error {
					sends = append(sends, int64(len(sends)+1))
					if len(sends) == 1 {
						return tc.firstErr
					}
					return nil
				},
			}

			newTestOutboxWorker(client, lock.run, repo).runCycle(auditableCtx(t))

			assert.Equal(t, tc.wantSends, sends)
		})
	}
}

func TestScoreOutboxWorker_OneReplicaSendsUnderTheLock(t *testing.T) {
	lock := &advisoryLock{}
	repo := lockedRepo(lock)
	repo.ListDueFunc = dueRows(models.ScoreOutboxRow{ID: 1})
	repo.MarkSentFunc = func(context.Context, int64, time.Time) error { return nil }

	var replicaB *scoreOutboxWorker
	clientA := &clientmocks.ScoreWriterClientMock{
		DeleteScoresFunc: func(ctx context.Context, _ scorewritersvc.DeleteScoresRequest) error {
			// Replica B runs its cycle while replica A holds the lock mid-send.
			replicaB.runCycle(ctx)
			return nil
		},
	}
	clientB := &clientmocks.ScoreWriterClientMock{
		DeleteScoresFunc: func(context.Context, scorewritersvc.DeleteScoresRequest) error { return nil },
	}
	replicaA := newTestOutboxWorker(clientA, lock.run, repo)
	replicaB = newTestOutboxWorker(clientB, lock.run, repo)

	replicaA.runCycle(auditableCtx(t))

	assert.Len(t, clientA.DeleteScoresCalls(), 1)
	assert.Empty(t, clientB.DeleteScoresCalls(), "replica B must not send while A holds the lock")
	assert.Len(t, repo.ListDueCalls(), 1)

	// A's transaction ended, so B takes the lock on its next cycle.
	replicaB.runCycle(auditableCtx(t))
	assert.Len(t, clientB.DeleteScoresCalls(), 1)
}

func TestScoreOutboxWorker_SendsNothingWhenTheLockFails(t *testing.T) {
	repo := &repomocks.ScoreOutboxRepositoryMock{
		WithTxFunc: func(*gorm.DB) repositories.ScoreOutboxRepository {
			return &repomocks.ScoreOutboxRepositoryMock{
				TryLockFunc: func(context.Context) (bool, error) { return false, errors.New("database unavailable") },
			}
		},
		// ListDueFunc nil: no rows may be read without the lock.
	}

	newTestOutboxWorker(&clientmocks.ScoreWriterClientMock{}, (&advisoryLock{}).run, repo).runCycle(auditableCtx(t))

	assert.Empty(t, repo.ListDueCalls())
}
