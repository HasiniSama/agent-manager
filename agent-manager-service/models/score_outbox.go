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

package models

import (
	"time"

	"github.com/google/uuid"
)

// ScoreOutboxKind names the change a score outbox row reports to the score writer.
type ScoreOutboxKind string

const (
	ScoreOutboxMonitorDelete    ScoreOutboxKind = "monitor_delete"
	ScoreOutboxEvaluatorRemoved ScoreOutboxKind = "evaluator_removed"
)

// ScoreOutboxPayload says which scores the score writer should delete.
type ScoreOutboxPayload struct {
	OrgID            string      `json:"orgId"`
	MonitorIDs       []uuid.UUID `json:"monitorIds"`
	EvaluatorName    string      `json:"evaluatorName,omitempty"`
	RunStartedBefore *time.Time  `json:"runStartedBefore,omitempty"`
}

// ScoreOutboxRow is one score delete queued for the score writer.
type ScoreOutboxRow struct {
	ID            int64              `gorm:"column:id;primaryKey;autoIncrement"`
	Kind          ScoreOutboxKind    `gorm:"column:kind;not null"`
	Payload       ScoreOutboxPayload `gorm:"column:payload;type:jsonb;serializer:json;not null"`
	CreatedAt     time.Time          `gorm:"column:created_at;not null"`
	SentAt        *time.Time         `gorm:"column:sent_at"`
	Attempts      int                `gorm:"column:attempts;not null;default:0"`
	NextAttemptAt *time.Time         `gorm:"column:next_attempt_at"`
	ResendAfter   *time.Time         `gorm:"column:resend_after"`
}

func (ScoreOutboxRow) TableName() string { return "score_outbox" }

// ScoreOutboxBacklog counts the rows still waiting for their first send.
type ScoreOutboxBacklog struct {
	Depth          int64
	OldestUnsentAt *time.Time
}
