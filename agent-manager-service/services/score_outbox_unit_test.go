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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/wso2/agent-manager/agent-manager-service/clients/clientmocks"
	"github.com/wso2/agent-manager/agent-manager-service/config"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/repositories"
	"github.com/wso2/agent-manager/agent-manager-service/repositories/repomocks"
)

var outboxNow = time.Date(2026, 10, 19, 6, 0, 0, 0, time.UTC)

// outboxStore stands in for the database: rows inserted in a transaction are kept only when it commits.
type outboxStore struct {
	t         *testing.T
	committed []models.ScoreOutboxRow
	pending   []models.ScoreOutboxRow
	insertErr error
	commitErr error
	// txRepo is the outbox repository bound to the transaction; only it can insert.
	txRepo *repomocks.ScoreOutboxRepositoryMock
}

func newOutboxStore(t *testing.T) *outboxStore {
	s := &outboxStore{t: t}
	s.txRepo = &repomocks.ScoreOutboxRepositoryMock{
		InsertFunc: func(_ context.Context, rows []models.ScoreOutboxRow) error {
			if s.insertErr != nil {
				return s.insertErr
			}
			s.pending = append(s.pending, rows...)
			return nil
		},
	}
	return s
}

func (s *outboxStore) run(ctx context.Context, fn func(tx *gorm.DB) error) error {
	s.pending = nil
	err := sentinelTx(ctx, fn)
	if err == nil {
		err = s.commitErr
	}
	if err == nil {
		s.committed = append(s.committed, s.pending...)
	}
	s.pending = nil
	return err
}

func (s *outboxStore) outbox() *ScoreOutbox {
	repo := &repomocks.ScoreOutboxRepositoryMock{
		WithTxFunc: func(tx *gorm.DB) repositories.ScoreOutboxRepository {
			assert.Same(s.t, txSentinel, tx)
			return s.txRepo
		},
	}
	cfg := config.Config{EvalRunToken: config.EvalRunTokenConfig{ActiveDeadlineSeconds: 1800}}
	o := NewScoreOutbox(s.run, repo, cfg)
	o.now = func() time.Time { return outboxNow }
	return o
}

// bindTx makes txMonitors the transaction-bound copy of base.
func bindTx(t *testing.T, base, txMonitors *repomocks.MonitorRepositoryMock) *repomocks.MonitorRepositoryMock {
	base.WithTxFunc = func(tx *gorm.DB) repositories.MonitorRepository {
		assert.Same(t, txSentinel, tx)
		return txMonitors
	}
	return base
}

// outboxMonitorManager wires a monitor manager whose monitors are found by name and have no runs or LLM provider.
func outboxMonitorManager(outbox *ScoreOutbox, monitors *repomocks.MonitorRepositoryMock, byName ...*models.Monitor) *monitorManagerService {
	monitors.GetMonitorByNameFunc = func(_, _, _, name string) (*models.Monitor, error) {
		for _, m := range byName {
			if m.Name == name {
				return m, nil
			}
		}
		return nil, gorm.ErrRecordNotFound
	}
	monitors.GetMonitorRunsByMonitorIDFunc = func(uuid.UUID) ([]models.MonitorRun, error) {
		return []models.MonitorRun{}, nil
	}
	monitors.GetLatestMonitorRunFunc = func(uuid.UUID) (*models.MonitorRun, error) {
		return nil, gorm.ErrRecordNotFound
	}
	custom := &repomocks.CustomEvaluatorRepositoryMock{
		GetByIdentifierFunc: func(_, identifier string) (*models.CustomEvaluator, error) {
			return &models.CustomEvaluator{Identifier: identifier, Type: models.CustomEvaluatorTypeCode}, nil
		},
	}
	return &monitorManagerService{
		logger:           discardLogger(),
		monitorRepo:      monitors,
		evaluatorService: NewEvaluatorManagerService(discardLogger(), custom, nil),
		monitorLLMMappingRepo: &repomocks.MonitorLLMMappingRepositoryMock{
			ListByMonitorIDFunc: func(context.Context, uuid.UUID) ([]models.MonitorLLMMapping, error) {
				return []models.MonitorLLMMapping{}, nil
			},
			DeleteByMonitorIDFunc: func(context.Context, *gorm.DB, uuid.UUID) error { return nil },
		},
		provisioner: &fakeProvisioner{IsThunderModeFunc: func() bool { return false }},
		scoreOutbox: outbox,
	}
}

func outboxEvaluators(names ...string) []models.MonitorEvaluator {
	evaluators := make([]models.MonitorEvaluator, len(names))
	for i, name := range names {
		evaluators[i] = models.MonitorEvaluator{Identifier: "custom-" + name, DisplayName: name}
	}
	return evaluators
}

func TestScoreOutbox_MonitorDeleteQueuesOneRowInItsTransaction(t *testing.T) {
	monitor := &models.Monitor{ID: uuid.New(), Name: "latency", OUID: "org-1", ProjectName: "proj", AgentName: "agent"}
	store := newOutboxStore(t)
	txMonitors := &repomocks.MonitorRepositoryMock{
		DeleteMonitorFunc: func(*models.Monitor) error { return nil },
	}
	// The plain repository has no DeleteMonitorFunc, so a delete outside the transaction panics.
	s := outboxMonitorManager(store.outbox(), bindTx(t, &repomocks.MonitorRepositoryMock{}, txMonitors), monitor)

	require.NoError(t, s.DeleteMonitor(auditableCtx(t), "org-1", "proj", "agent", "latency"))

	require.Len(t, txMonitors.DeleteMonitorCalls(), 1)
	require.Len(t, store.committed, 1)
	row := store.committed[0]
	assert.Equal(t, models.ScoreOutboxMonitorDelete, row.Kind)
	assert.Equal(t, models.ScoreOutboxPayload{OrgID: "org-1", MonitorIDs: []uuid.UUID{monitor.ID}}, row.Payload)
	assert.Equal(t, outboxNow, row.CreatedAt)
	assert.Equal(t, outboxNow, *row.NextAttemptAt)
	assert.Equal(t, outboxNow.Add(30*time.Minute+10*time.Minute), *row.ResendAfter, "resend after the run deadline plus 10 minutes")
	assert.Nil(t, row.SentAt)
}

func TestScoreOutbox_FailedMonitorDeleteQueuesNothing(t *testing.T) {
	failure := errors.New("database unavailable")
	for _, tc := range []struct {
		name                            string
		deleteErr, insertErr, commitErr error
	}{
		{name: "monitor row delete fails", deleteErr: failure},
		{name: "outbox insert fails", insertErr: failure},
		{name: "commit fails", commitErr: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			monitor := &models.Monitor{ID: uuid.New(), Name: "latency", OUID: "org-1"}
			store := newOutboxStore(t)
			store.insertErr, store.commitErr = tc.insertErr, tc.commitErr
			txMonitors := &repomocks.MonitorRepositoryMock{
				DeleteMonitorFunc: func(*models.Monitor) error { return tc.deleteErr },
			}
			s := outboxMonitorManager(store.outbox(), bindTx(t, &repomocks.MonitorRepositoryMock{}, txMonitors), monitor)

			err := s.DeleteMonitor(auditableCtx(t), "org-1", "proj", "agent", "latency")

			require.ErrorIs(t, err, failure)
			assert.Empty(t, store.committed)
		})
	}
}

func TestScoreOutbox_EvaluatorRemoval(t *testing.T) {
	newMonitor := func() *models.Monitor {
		return &models.Monitor{
			ID: uuid.New(), Name: "quality", OUID: "org-1", ProjectName: "proj", AgentName: "agent",
			Type: models.MonitorTypeFuture, Evaluators: outboxEvaluators("relevance", "tone", "safety"),
		}
	}

	t.Run("dropping two evaluators queues one row each with the update", func(t *testing.T) {
		monitor := newMonitor()
		store := newOutboxStore(t)
		txMonitors := &repomocks.MonitorRepositoryMock{
			UpdateMonitorFunc: func(*models.Monitor) error { return nil },
		}
		s := outboxMonitorManager(store.outbox(), bindTx(t, &repomocks.MonitorRepositoryMock{}, txMonitors), monitor)
		kept := outboxEvaluators("tone")

		_, err := s.UpdateMonitor(auditableCtx(t), "org-1", "proj", "agent", "quality",
			&models.UpdateMonitorRequest{Evaluators: &kept})

		require.NoError(t, err)
		require.Len(t, txMonitors.UpdateMonitorCalls(), 1)
		require.Len(t, store.committed, 2)
		for i, name := range []string{"relevance", "safety"} {
			row := store.committed[i]
			assert.Equal(t, models.ScoreOutboxEvaluatorRemoved, row.Kind)
			assert.Equal(t, "org-1", row.Payload.OrgID)
			assert.Equal(t, []uuid.UUID{monitor.ID}, row.Payload.MonitorIDs)
			assert.Equal(t, name, row.Payload.EvaluatorName)
			require.NotNil(t, row.Payload.RunStartedBefore)
			assert.Equal(t, outboxNow, *row.Payload.RunStartedBefore, "runs started before the removal")
		}
	})

	t.Run("an update that keeps every evaluator queues nothing", func(t *testing.T) {
		for name, req := range map[string]*models.UpdateMonitorRequest{
			"evaluators untouched": {DisplayName: strPtr("Quality")},
			"same evaluators":      {Evaluators: func() *[]models.MonitorEvaluator { e := outboxEvaluators("safety", "relevance", "tone"); return &e }()},
		} {
			t.Run(name, func(t *testing.T) {
				store := newOutboxStore(t)
				// No WithTxFunc: an update that needs no score delete opens no transaction.
				monitors := &repomocks.MonitorRepositoryMock{
					UpdateMonitorFunc: func(*models.Monitor) error { return nil },
				}
				s := outboxMonitorManager(store.outbox(), monitors, newMonitor())

				_, err := s.UpdateMonitor(auditableCtx(t), "org-1", "proj", "agent", "quality", req)

				require.NoError(t, err)
				assert.Len(t, monitors.UpdateMonitorCalls(), 1)
				assert.Empty(t, store.committed)
			})
		}
	})

	t.Run("a failed transaction queues nothing", func(t *testing.T) {
		failure := errors.New("insert failed")
		store := newOutboxStore(t)
		store.insertErr = failure
		txMonitors := &repomocks.MonitorRepositoryMock{
			UpdateMonitorFunc: func(*models.Monitor) error { return nil },
		}
		s := outboxMonitorManager(store.outbox(), bindTx(t, &repomocks.MonitorRepositoryMock{}, txMonitors), newMonitor())
		kept := outboxEvaluators("tone")

		_, err := s.UpdateMonitor(auditableCtx(t), "org-1", "proj", "agent", "quality",
			&models.UpdateMonitorRequest{Evaluators: &kept})

		require.ErrorIs(t, err, failure)
		assert.Empty(t, store.committed)
	})
}

func TestScoreOutbox_AgentDeleteQueuesOneRowForEveryMonitor(t *testing.T) {
	first := &models.Monitor{ID: uuid.New(), Name: "first", OUID: "org-1", ProjectName: "proj", AgentName: "agent"}
	second := &models.Monitor{ID: uuid.New(), Name: "second", OUID: "org-1", ProjectName: "proj", AgentName: "agent"}
	store := newOutboxStore(t)
	// The monitors are listed inside the transaction that queues their score delete.
	txMonitors := &repomocks.MonitorRepositoryMock{
		ListMonitorsByAgentFunc: func(ouID, projectName, agentName string) ([]models.Monitor, error) {
			assert.Equal(t, []string{"org-1", "proj", "agent"}, []string{ouID, projectName, agentName})
			return []models.Monitor{*first, *second}, nil
		},
	}
	monitors := &repomocks.MonitorRepositoryMock{
		DeleteMonitorFunc: func(m *models.Monitor) error {
			if m.ID == second.ID {
				return errors.New("delete failed")
			}
			return nil
		},
	}
	s := outboxMonitorManager(store.outbox(), bindTx(t, monitors, txMonitors), first, second)

	err := s.DeleteMonitorsByAgent(auditableCtx(t), "org-1", "proj", "agent")

	require.Error(t, err, "the second monitor's delete failure is still reported")
	assert.Len(t, monitors.DeleteMonitorCalls(), 2, "each monitor row is deleted outside the queuing transaction")
	require.Len(t, store.committed, 1)
	assert.Equal(t, models.ScoreOutboxMonitorDelete, store.committed[0].Kind)
	assert.Equal(t, models.ScoreOutboxPayload{OrgID: "org-1", MonitorIDs: []uuid.UUID{first.ID, second.ID}},
		store.committed[0].Payload)
}

func TestScoreOutbox_AgentDeleteWithNoMonitorsQueuesNothing(t *testing.T) {
	store := newOutboxStore(t)
	txMonitors := &repomocks.MonitorRepositoryMock{
		ListMonitorsByAgentFunc: func(_, _, _ string) ([]models.Monitor, error) {
			return []models.Monitor{}, nil
		},
	}
	s := outboxMonitorManager(store.outbox(), bindTx(t, &repomocks.MonitorRepositoryMock{}, txMonitors))

	require.NoError(t, s.DeleteMonitorsByAgent(auditableCtx(t), "org-1", "proj", "agent"))
	assert.Empty(t, store.committed)
	assert.Empty(t, store.txRepo.InsertCalls())
}

func TestScoreOutbox_AgentDeleteKeepsMonitorsWhenQueuingFails(t *testing.T) {
	failure := errors.New("insert failed")
	store := newOutboxStore(t)
	store.insertErr = failure
	txMonitors := &repomocks.MonitorRepositoryMock{
		ListMonitorsByAgentFunc: func(_, _, _ string) ([]models.Monitor, error) {
			return []models.Monitor{{ID: uuid.New(), Name: "first"}}, nil
		},
	}
	// No DeleteMonitorFunc: deleting a monitor whose scores were not queued panics.
	s := outboxMonitorManager(store.outbox(), bindTx(t, &repomocks.MonitorRepositoryMock{}, txMonitors))

	err := s.DeleteMonitorsByAgent(auditableCtx(t), "org-1", "proj", "agent")

	require.ErrorIs(t, err, failure)
	assert.Empty(t, store.committed)
}

func TestScoreOutbox_ProjectDelete(t *testing.T) {
	newOC := func(onDelete func()) *clientmocks.OpenChoreoClientMock {
		oc := &clientmocks.OpenChoreoClientMock{
			GetOrganizationFunc: okOrg(),
			CountProjectComponentsFunc: func(context.Context, string, string) (int, error) {
				return 0, nil
			},
		}
		if onDelete != nil {
			oc.DeleteProjectFunc = func(context.Context, string, string) error {
				onDelete()
				return nil
			}
		}
		return oc
	}

	t.Run("queues every monitor of the project, 100 IDs per row, before deleting it", func(t *testing.T) {
		ids := make([]uuid.UUID, 250)
		for i := range ids {
			ids[i] = uuid.New()
		}
		store := newOutboxStore(t)
		store.txRepo.ProjectMonitorIDsFunc = func(_ context.Context, ouID, projectName string) ([]uuid.UUID, error) {
			assert.Equal(t, []string{"org-1", "proj"}, []string{ouID, projectName})
			return ids, nil
		}
		queuedAtDelete := -1
		oc := newOC(func() { queuedAtDelete = len(store.committed) })

		err := NewInfraResourceManager(oc, discardLogger(), WithProjectScoreOutbox(store.outbox())).
			DeleteProject(auditableCtx(t), "org-1", "proj")

		require.NoError(t, err)
		assert.Equal(t, 3, queuedAtDelete, "rows are committed before the project is deleted")
		require.Len(t, store.committed, 3)
		var queued []uuid.UUID
		for i, want := range []int{100, 100, 50} {
			row := store.committed[i]
			assert.Equal(t, models.ScoreOutboxMonitorDelete, row.Kind)
			assert.Equal(t, "org-1", row.Payload.OrgID)
			assert.Len(t, row.Payload.MonitorIDs, want)
			queued = append(queued, row.Payload.MonitorIDs...)
		}
		assert.Equal(t, ids, queued)
	})

	t.Run("a project with no monitors queues nothing", func(t *testing.T) {
		store := newOutboxStore(t)
		store.txRepo.ProjectMonitorIDsFunc = func(context.Context, string, string) ([]uuid.UUID, error) {
			return []uuid.UUID{}, nil
		}
		deleted := false

		err := NewInfraResourceManager(newOC(func() { deleted = true }), discardLogger(), WithProjectScoreOutbox(store.outbox())).
			DeleteProject(auditableCtx(t), "org-1", "proj")

		require.NoError(t, err)
		assert.True(t, deleted)
		assert.Empty(t, store.committed)
	})

	t.Run("refuses the delete when the score deletes cannot be queued", func(t *testing.T) {
		failure := errors.New("insert failed")
		store := newOutboxStore(t)
		store.insertErr = failure
		store.txRepo.ProjectMonitorIDsFunc = func(context.Context, string, string) ([]uuid.UUID, error) {
			return []uuid.UUID{uuid.New()}, nil
		}

		// DeleteProjectFunc nil: the project must not be deleted.
		err := NewInfraResourceManager(newOC(nil), discardLogger(), WithProjectScoreOutbox(store.outbox())).
			DeleteProject(auditableCtx(t), "org-1", "proj")

		require.ErrorIs(t, err, failure)
		assert.Empty(t, store.committed)
	})
}
