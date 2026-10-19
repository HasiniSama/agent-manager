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

package config

import "testing"

func TestLoadScoreWriter_InternalRouteDefaults(t *testing.T) {
	setScoreWriterEnv(t, map[string]string{"SCORE_WRITER_INTERNAL_AUDIENCE": "", "SCORE_WRITER_BACKFILL_ENABLED": ""})

	cfg, err := LoadScoreWriter()
	if err != nil {
		t.Fatalf("LoadScoreWriter: %v", err)
	}
	if cfg.InternalAuth.Audience != "" {
		t.Errorf("internal audience = %q, want empty so the writer uses %s", cfg.InternalAuth.Audience, DefaultScoreWriterAudience)
	}
	if cfg.BackfillEnabled {
		t.Error("backfill is on by default")
	}
}

func TestLoadScoreWriter_InternalRouteValues(t *testing.T) {
	setScoreWriterEnv(t, map[string]string{
		"SCORE_WRITER_INTERNAL_AUDIENCE": " urn:test:score-writer ",
		"SCORE_WRITER_BACKFILL_ENABLED":  "true",
	})

	cfg, err := LoadScoreWriter()
	if err != nil {
		t.Fatalf("LoadScoreWriter: %v", err)
	}
	if cfg.InternalAuth.Audience != "urn:test:score-writer" {
		t.Errorf("internal audience = %q, want urn:test:score-writer", cfg.InternalAuth.Audience)
	}
	if !cfg.BackfillEnabled {
		t.Error("backfill is off with SCORE_WRITER_BACKFILL_ENABLED=true")
	}
}
