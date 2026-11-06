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
	"time"

	"github.com/google/uuid"

	"github.com/wso2/agent-manager/agent-manager-service/models"
	"github.com/wso2/agent-manager/agent-manager-service/orgctx"
	"github.com/wso2/agent-manager/agent-manager-service/repositories"
	"github.com/wso2/agent-manager/agent-manager-service/spec"
	"github.com/wso2/agent-manager/agent-manager-service/utils"
)

// runTokenExpiryMargin is added to the run timeout to get the token's expiry.
const runTokenExpiryMargin = 5 * time.Minute

// MonitorRunTokenService mints the tokens monitor runs use to write evaluation spans
type MonitorRunTokenService struct {
	monitorRepo  repositories.MonitorRepository
	tokenManager AgentTokenManagerService
	logger       *slog.Logger
}

// NewMonitorRunTokenService creates a new monitor run token service
func NewMonitorRunTokenService(
	monitorRepo repositories.MonitorRepository,
	tokenManager AgentTokenManagerService,
	logger *slog.Logger,
) *MonitorRunTokenService {
	return &MonitorRunTokenService{
		monitorRepo:  monitorRepo,
		tokenManager: tokenManager,
		logger:       logger,
	}
}

// IssueRunToken mints a token that lets a pending or running run write spans for its monitor's agent.
// callerOUID is the organization asserted by the publisher token.
func (s *MonitorRunTokenService) IssueRunToken(
	ctx context.Context,
	monitorID, runID uuid.UUID,
	callerOUID string,
) (*spec.TokenResponse, error) {
	monitor, run, err := authorizeRun(s.monitorRepo, s.logger, monitorID, runID, callerOUID)
	if err != nil {
		return nil, err
	}

	if run.Status != models.RunStatusPending && run.Status != models.RunStatusRunning {
		return nil, fmt.Errorf("%w: run %s is %s", utils.ErrConflict, runID, run.Status)
	}
	if run.StartedAt == nil {
		return nil, fmt.Errorf("%w: run %s has no start time", utils.ErrConflict, runID)
	}
	// The scheduler fails a run that is still active after runStuckTimeout.
	expiresAt := run.StartedAt.Add(runStuckTimeout + runTokenExpiryMargin)
	if !time.Now().Before(expiresAt) {
		return nil, fmt.Errorf("%w: run %s is past its timeout", utils.ErrConflict, runID)
	}

	ctx = orgctx.WithResolvedOrg(ctx, orgctx.ResolvedOrg{OUID: monitor.OUID})
	return s.tokenManager.GenerateRunToken(ctx, GenerateRunTokenRequest{
		OrgId:           monitor.OUID,
		ProjectName:     monitor.ProjectName,
		AgentName:       monitor.AgentName,
		ComponentUid:    monitor.AgentID,
		EnvironmentUid:  monitor.EnvironmentID,
		EnvironmentName: monitor.EnvironmentName,
		MonitorId:       monitor.ID.String(),
		RunId:           run.ID.String(),
		ExpiresAt:       expiresAt,
	})
}
