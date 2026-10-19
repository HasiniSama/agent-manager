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

import (
	"strings"
	"testing"
)

func TestValidateScoreWriterConfig(t *testing.T) {
	tests := []struct {
		name        string
		cfg         ScoreWriterConfig
		errContains string
	}{
		{name: "no writer URL is valid", cfg: ScoreWriterConfig{ClientTimeout: "10s"}},
		{name: "https writer URL is valid", cfg: ScoreWriterConfig{InternalURL: "https://amp-score-writer:9199", ClientTimeout: "10s"}},
		{name: "plain http is rejected", cfg: ScoreWriterConfig{InternalURL: "http://amp-score-writer:9199", ClientTimeout: "10s"}, errContains: "must use https"},
		{name: "a URL without a host is rejected", cfg: ScoreWriterConfig{InternalURL: "https://", ClientTimeout: "10s"}, errContains: "non-empty host"},
		{name: "a malformed timeout is rejected", cfg: ScoreWriterConfig{ClientTimeout: "ten seconds"}, errContains: "SCORE_WRITER_CLIENT_TIMEOUT"},
		{name: "a zero timeout is rejected", cfg: ScoreWriterConfig{ClientTimeout: "0s"}, errContains: "SCORE_WRITER_CLIENT_TIMEOUT"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &configReader{}
			validateScoreWriterConfig(&Config{ScoreWriter: tc.cfg}, r)
			assertSingleError(t, r, tc.errContains)
		})
	}
}

func TestValidateEvalRunTokenConfig(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deadline    int
		errContains string
	}{
		{name: "the chart default is valid", deadline: 1800},
		{name: "zero is rejected", deadline: 0, errContains: "EVAL_RUN_ACTIVE_DEADLINE_SECONDS"},
		{name: "negative is rejected", deadline: -1, errContains: "EVAL_RUN_ACTIVE_DEADLINE_SECONDS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &configReader{}
			validateEvalRunTokenConfig(&Config{EvalRunToken: EvalRunTokenConfig{ActiveDeadlineSeconds: tc.deadline}}, r)
			assertSingleError(t, r, tc.errContains)
		})
	}
}

func TestLoadEnvs_ScoreWriterDefaults(t *testing.T) {
	for k, v := range map[string]string{
		"OPEN_CHOREO_BASE_URL": "http://localhost/api/v1",
		"DB_HOST":              "localhost",
		"DB_USER":              "unit",
		"DB_PASSWORD":          "unit",
		"DB_NAME":              "unit",
	} {
		t.Setenv(k, v)
	}
	loadEnvs()
	cfg := GetConfig()
	if cfg.ScoreWriter != (ScoreWriterConfig{ClientTimeout: "10s"}) {
		t.Errorf("score writer defaults = %+v, want an empty URL and a 10s timeout", cfg.ScoreWriter)
	}
	if cfg.EvalRunToken.ActiveDeadlineSeconds != 1800 {
		t.Errorf("run deadline default = %d, want 1800", cfg.EvalRunToken.ActiveDeadlineSeconds)
	}
}

// assertSingleError expects no error when errContains is empty, else exactly one error containing it.
func assertSingleError(t *testing.T, r *configReader, errContains string) {
	t.Helper()
	if errContains == "" {
		if len(r.errors) != 0 {
			t.Fatalf("expected no errors, got %v", r.errors)
		}
		return
	}
	if len(r.errors) != 1 || !strings.Contains(r.errors[0].Error(), errContains) {
		t.Fatalf("expected one error containing %q, got %v", errContains, r.errors)
	}
}
