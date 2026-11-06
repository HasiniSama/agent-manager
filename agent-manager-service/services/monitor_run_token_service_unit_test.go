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

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/wso2/agent-manager/agent-manager-service/audit"
	"github.com/wso2/agent-manager/agent-manager-service/clients/clientmocks"
	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/repositories/repomocks"
	"github.com/wso2/agent-manager/agent-manager-service/utils"
)

func runTokenMonitor() *models.Monitor {
	return &models.Monitor{
		ID:              uuid.New(),
		OUID:            "org-123",
		ProjectName:     "proj",
		AgentName:       "my-agent",
		AgentID:         "comp-uuid",
		EnvironmentName: "dev",
		EnvironmentID:   "env-uuid",
	}
}

func runTokenRun(monitorID uuid.UUID, status string, startedAt time.Time) *models.MonitorRun {
	return &models.MonitorRun{ID: uuid.New(), MonitorID: monitorID, Status: status, StartedAt: &startedAt}
}

func runTokenMonitorRepo(monitor *models.Monitor, run *models.MonitorRun) *repomocks.MonitorRepositoryMock {
	return &repomocks.MonitorRepositoryMock{
		GetMonitorByIDFunc: func(id uuid.UUID) (*models.Monitor, error) {
			if id != monitor.ID {
				return nil, gorm.ErrRecordNotFound
			}
			return monitor, nil
		},
		GetMonitorRunByIDFunc: func(runID, monitorID uuid.UUID) (*models.MonitorRun, error) {
			if runID != run.ID || monitorID != run.MonitorID {
				return nil, gorm.ErrRecordNotFound
			}
			return run, nil
		},
	}
}

func TestMonitorRunTokenService_IssueRunToken(t *testing.T) {
	t.Run("claims carry the monitor's agent and run", func(t *testing.T) {
		monitor := runTokenMonitor()
		startedAt := time.Now().Add(-time.Minute).Truncate(time.Second)
		run := runTokenRun(monitor.ID, models.RunStatusRunning, startedAt)
		svc := NewMonitorRunTokenService(runTokenMonitorRepo(monitor, run),
			newTokenManager(t, fullClientMock("unused", "unused", "proj-uuid")), discardLogger())

		resp, err := svc.IssueRunToken(auditableCtx(t), monitor.ID, run.ID, "org-123")

		require.NoError(t, err)
		claims := &RunTokenClaims{}
		_, _, err = jwt.NewParser().ParseUnverified(resp.Token, claims)
		require.NoError(t, err)
		assert.Equal(t, "comp-uuid", claims.ComponentUid)
		assert.Equal(t, "env-uuid", claims.EnvironmentUid)
		assert.Equal(t, "proj-uuid", claims.ProjectUid)
		assert.Equal(t, "org-123", claims.OrgId)
		assert.Equal(t, "acme-namespace", claims.Namespace)
		assert.Equal(t, "my-agent", claims.Subject)
		assert.Equal(t, run.ID.String(), claims.EvalRunId)
		assert.Equal(t, monitor.ID.String(), claims.EvalMonitorId)
		assert.NotEmpty(t, claims.ID)
	})

	t.Run("expires at the run timeout plus margin", func(t *testing.T) {
		monitor := runTokenMonitor()
		startedAt := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
		run := runTokenRun(monitor.ID, models.RunStatusPending, startedAt)
		svc := NewMonitorRunTokenService(runTokenMonitorRepo(monitor, run),
			newTokenManager(t, fullClientMock("c", "e", "p")), discardLogger())

		resp, err := svc.IssueRunToken(auditableCtx(t), monitor.ID, run.ID, "org-123")

		require.NoError(t, err)
		want := startedAt.Add(runStuckTimeout + runTokenExpiryMargin)
		assert.Equal(t, want.Unix(), resp.ExpiresAt)
		claims := &RunTokenClaims{}
		_, _, err = jwt.NewParser().ParseUnverified(resp.Token, claims)
		require.NoError(t, err)
		require.NotNil(t, claims.ExpiresAt)
		assert.Equal(t, want.Unix(), claims.ExpiresAt.Unix())
	})

	t.Run("refuses another org's run", func(t *testing.T) {
		monitor := runTokenMonitor()
		run := runTokenRun(monitor.ID, models.RunStatusRunning, time.Now())
		svc := NewMonitorRunTokenService(runTokenMonitorRepo(monitor, run), &stubTokenManager{}, discardLogger())

		_, err := svc.IssueRunToken(auditableCtx(t), monitor.ID, run.ID, "attacker-org")

		assert.ErrorIs(t, err, utils.ErrForbidden)
	})

	t.Run("refuses a caller with no org", func(t *testing.T) {
		monitor := runTokenMonitor()
		run := runTokenRun(monitor.ID, models.RunStatusRunning, time.Now())
		svc := NewMonitorRunTokenService(runTokenMonitorRepo(monitor, run), &stubTokenManager{}, discardLogger())

		_, err := svc.IssueRunToken(auditableCtx(t), monitor.ID, run.ID, "")

		assert.ErrorIs(t, err, utils.ErrForbidden)
	})

	t.Run("refuses finished runs", func(t *testing.T) {
		for _, status := range []string{models.RunStatusSuccess, models.RunStatusFailed} {
			monitor := runTokenMonitor()
			run := runTokenRun(monitor.ID, status, time.Now())
			svc := NewMonitorRunTokenService(runTokenMonitorRepo(monitor, run), &stubTokenManager{}, discardLogger())

			_, err := svc.IssueRunToken(auditableCtx(t), monitor.ID, run.ID, "org-123")

			assert.ErrorIs(t, err, utils.ErrConflict, status)
		}
	})

	t.Run("refuses a run past its timeout", func(t *testing.T) {
		monitor := runTokenMonitor()
		startedAt := time.Now().Add(-(runStuckTimeout + runTokenExpiryMargin + time.Minute))
		run := runTokenRun(monitor.ID, models.RunStatusRunning, startedAt)
		svc := NewMonitorRunTokenService(runTokenMonitorRepo(monitor, run), &stubTokenManager{}, discardLogger())

		_, err := svc.IssueRunToken(auditableCtx(t), monitor.ID, run.ID, "org-123")

		assert.ErrorIs(t, err, utils.ErrConflict)
	})

	t.Run("refuses an unknown run", func(t *testing.T) {
		monitor := runTokenMonitor()
		run := runTokenRun(monitor.ID, models.RunStatusRunning, time.Now())
		svc := NewMonitorRunTokenService(runTokenMonitorRepo(monitor, run), &stubTokenManager{}, discardLogger())

		_, err := svc.IssueRunToken(auditableCtx(t), monitor.ID, uuid.New(), "org-123")

		assert.ErrorIs(t, err, utils.ErrNotFound)
	})

	t.Run("refuses another monitor's run", func(t *testing.T) {
		monitor := runTokenMonitor()
		run := runTokenRun(uuid.New(), models.RunStatusRunning, time.Now())
		svc := NewMonitorRunTokenService(runTokenMonitorRepo(monitor, run), &stubTokenManager{}, discardLogger())

		_, err := svc.IssueRunToken(auditableCtx(t), monitor.ID, run.ID, "org-123")

		assert.ErrorIs(t, err, utils.ErrNotFound)
	})

	t.Run("refuses an unknown monitor", func(t *testing.T) {
		monitor := runTokenMonitor()
		run := runTokenRun(monitor.ID, models.RunStatusRunning, time.Now())
		svc := NewMonitorRunTokenService(runTokenMonitorRepo(monitor, run), &stubTokenManager{}, discardLogger())

		_, err := svc.IssueRunToken(auditableCtx(t), uuid.New(), run.ID, "org-123")

		assert.ErrorIs(t, err, utils.ErrNotFound)
	})

	t.Run("does not mask a lookup failure as not found", func(t *testing.T) {
		boom := errors.New("connection refused")
		repo := &repomocks.MonitorRepositoryMock{
			GetMonitorByIDFunc: func(uuid.UUID) (*models.Monitor, error) { return nil, boom },
		}
		svc := NewMonitorRunTokenService(repo, &stubTokenManager{}, discardLogger())

		_, err := svc.IssueRunToken(auditableCtx(t), uuid.New(), uuid.New(), "org-123")

		assert.ErrorIs(t, err, boom)
		assert.NotErrorIs(t, err, utils.ErrNotFound)
	})

	t.Run("refuses without an audit recorder", func(t *testing.T) {
		monitor := runTokenMonitor()
		run := runTokenRun(monitor.ID, models.RunStatusRunning, time.Now())
		svc := NewMonitorRunTokenService(runTokenMonitorRepo(monitor, run),
			newTokenManager(t, fullClientMock("c", "e", "p")), discardLogger())

		resp, err := svc.IssueRunToken(context.Background(), monitor.ID, run.ID, "org-123")

		require.ErrorIs(t, err, audit.ErrRecorderUnavailable)
		assert.Nil(t, resp)
	})
}

func TestAgentTokenManager_GenerateRunToken_PropagatesGetProjectError(t *testing.T) {
	boom := errors.New("openchoreo unavailable")
	oc := &clientmocks.OpenChoreoClientMock{
		GetProjectFunc: func(context.Context, string, string) (*models.ProjectResponse, error) { return nil, boom },
	}
	svc := newTokenManager(t, oc)

	_, err := svc.GenerateRunToken(auditableCtx(t), GenerateRunTokenRequest{
		OrgId:       "org-123",
		ProjectName: "proj",
		ExpiresAt:   time.Now().Add(time.Hour),
	})

	assert.ErrorIs(t, err, boom)
}

func TestAgentTokenManager_GenerateToken_HasNoRunClaims(t *testing.T) {
	svc := newTokenManager(t, fullClientMock("c", "e", "p"))

	resp, err := svc.GenerateToken(auditableCtx(t), GenerateTokenRequest{
		OrgName:     "acme",
		ProjectName: "proj",
		AgentName:   "agent",
		Environment: "dev",
		OrgId:       "org-123",
	})

	require.NoError(t, err)
	claims := jwt.MapClaims{}
	_, _, err = jwt.NewParser().ParseUnverified(resp.Token, claims)
	require.NoError(t, err)
	assert.NotContains(t, claims, "eval_run_id")
	assert.NotContains(t, claims, "eval_monitor_id")
}
