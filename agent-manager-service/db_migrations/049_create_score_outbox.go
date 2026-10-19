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

package dbmigrations

import (
	"gorm.io/gorm"
)

// score_outbox queues the score deletes agent-manager-service sends to the score writer.
var migration049 = migration{
	ID: 49,
	Migrate: func(db *gorm.DB) error {
		return db.Transaction(func(tx *gorm.DB) error {
			createOutbox := `
			CREATE TABLE IF NOT EXISTS score_outbox (
				id              BIGSERIAL PRIMARY KEY,
				kind            VARCHAR(32) NOT NULL
					CHECK (kind IN ('monitor_delete', 'evaluator_removed')),
				payload         JSONB NOT NULL,
				created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				sent_at         TIMESTAMPTZ,
				attempts        INTEGER NOT NULL DEFAULT 0,
				next_attempt_at TIMESTAMPTZ,
				resend_after    TIMESTAMPTZ
			)`
			createDueIndex := `
			CREATE INDEX IF NOT EXISTS idx_score_outbox_due
				ON score_outbox (sent_at, next_attempt_at)`
			return runSQL(tx, createOutbox, createDueIndex)
		})
	},
}
