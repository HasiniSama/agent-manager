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

func TestValidateScoreWriterConfig_Audience(t *testing.T) {
	for _, tc := range []struct {
		name        string
		audience    string
		errContains string
	}{
		{name: "empty uses the default", audience: ""},
		{name: "a urn is valid", audience: "urn:wso2:amp:score-writer"},
		{name: "an https URL is valid", audience: "https://scores.example.com"},
		{name: "a bare name is rejected", audience: "score-writer", errContains: "SCORE_WRITER_AUDIENCE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &configReader{}
			validateScoreWriterConfig(&Config{ScoreWriter: ScoreWriterConfig{ClientTimeout: "10s", Audience: tc.audience}}, r)
			assertSingleError(t, r, tc.errContains)
		})
	}
}

func TestLoadEnvs_ScoreWriterAudience(t *testing.T) {
	for k, v := range map[string]string{
		"OPEN_CHOREO_BASE_URL":  "http://localhost/api/v1",
		"DB_HOST":               "localhost",
		"DB_USER":               "unit",
		"DB_PASSWORD":           "unit",
		"DB_NAME":               "unit",
		"SCORE_WRITER_AUDIENCE": " urn:test:score-writer ",
	} {
		t.Setenv(k, v)
	}
	loadEnvs()
	if got := GetConfig().ScoreWriter.Audience; got != "urn:test:score-writer" {
		t.Errorf("score writer audience = %q, want urn:test:score-writer", got)
	}
}
