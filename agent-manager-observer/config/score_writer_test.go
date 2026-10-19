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
	"reflect"
	"strings"
	"testing"
)

// requiredScoreWriterEnv is the smallest environment the writer starts with.
var requiredScoreWriterEnv = map[string]string{
	"SCORE_WRITER_TLS_CERT_FILE": "/tls/tls.crt",
	"SCORE_WRITER_TLS_KEY_FILE":  "/tls/tls.key",
	"SCORE_DB_HOST":              "db.example.com",
	"SCORE_DB_PASSWORD_FILE":     "/db/password",
	"SCORE_DB_CA_FILE":           "/db/ca.crt",
	"EVAL_RUN_JWKS_URL":          "https://amp-api.example.com/auth/eval-run/jwks.json",
	"KEY_MANAGER_JWKS_URL":       "https://thunder.example.com/oauth2/jwks",
	"KEY_MANAGER_ISSUER":         "https://thunder.example.com",
}

// optionalScoreWriterEnv lists the variables that have defaults.
var optionalScoreWriterEnv = []string{
	"SCORE_WRITER_PUBLISH_PORT", "SCORE_WRITER_INTERNAL_PORT", "SCORE_DB_PORT", "SCORE_DB_NAME",
	"SCORE_DB_USER", "EVAL_RUN_ISSUER", "EVAL_RUN_AUDIENCE", "SCORE_WRITER_INTERNAL_CLIENT_ID", "LOG_LEVEL",
	"SCORE_RETENTION_DAYS",
}

func setScoreWriterEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	for _, key := range optionalScoreWriterEnv {
		t.Setenv(key, "")
	}
	for key, value := range requiredScoreWriterEnv {
		t.Setenv(key, value)
	}
	for key, value := range overrides {
		t.Setenv(key, value)
	}
}

func TestLoadScoreWriter_EveryValue(t *testing.T) {
	setScoreWriterEnv(t, map[string]string{
		"SCORE_WRITER_PUBLISH_PORT":       "8443",
		"SCORE_WRITER_INTERNAL_PORT":      "8444",
		"SCORE_DB_PORT":                   "6432",
		"SCORE_DB_NAME":                   "scores",
		"SCORE_DB_USER":                   "writer",
		"EVAL_RUN_ISSUER":                 "urn:test:issuer",
		"EVAL_RUN_AUDIENCE":               "urn:test:audience",
		"KEY_MANAGER_ISSUER":              "https://a.example.com, https://b.example.com",
		"SCORE_WRITER_INTERNAL_CLIENT_ID": "amp-service",
		"SCORE_RETENTION_DAYS":            "14",
		"LOG_LEVEL":                       "DEBUG",
	})

	cfg, err := LoadScoreWriter()
	if err != nil {
		t.Fatalf("LoadScoreWriter: %v", err)
	}
	want := &ScoreWriterConfig{
		PublishPort:  8443,
		InternalPort: 8444,
		TLS:          TLSFiles{CertFile: "/tls/tls.crt", KeyFile: "/tls/tls.key"},
		Database: ScoreDatabaseConfig{
			Host: "db.example.com", Port: 6432, Name: "scores", User: "writer",
			PasswordFile: "/db/password", CAFile: "/db/ca.crt",
		},
		RunToken: RunTokenConfig{
			JWKSURL:  "https://amp-api.example.com/auth/eval-run/jwks.json",
			Issuer:   "urn:test:issuer",
			Audience: "urn:test:audience",
		},
		InternalAuth: InternalAuthConfig{
			JWKSURL:  "https://thunder.example.com/oauth2/jwks",
			Issuer:   []string{"https://a.example.com", "https://b.example.com"},
			ClientID: "amp-service",
		},
		RetentionDays: 14,
		LogLevel:      "DEBUG",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("config mismatch\n got: %+v\nwant: %+v", cfg, want)
	}
}

func TestLoadScoreWriter_Defaults(t *testing.T) {
	setScoreWriterEnv(t, nil)

	cfg, err := LoadScoreWriter()
	if err != nil {
		t.Fatalf("LoadScoreWriter: %v", err)
	}
	if cfg.PublishPort != 9099 || cfg.InternalPort != 9199 {
		t.Errorf("ports = %d, %d; want 9099, 9199", cfg.PublishPort, cfg.InternalPort)
	}
	if cfg.Database.Port != 5432 || cfg.Database.Name != "amp_scores" || cfg.Database.User != "amp_scores_writer" {
		t.Errorf("database defaults = %+v", cfg.Database)
	}
	if cfg.RunToken.Issuer != "urn:wso2:amp:eval-run" || cfg.RunToken.Audience != "urn:wso2:amp:eval-run" {
		t.Errorf("run token defaults = %+v", cfg.RunToken)
	}
	if cfg.InternalAuth.ClientID != "amp-api-client" {
		t.Errorf("internal client ID = %q, want amp-api-client", cfg.InternalAuth.ClientID)
	}
	if cfg.RetentionDays != 30 {
		t.Errorf("retention days = %d, want 30", cfg.RetentionDays)
	}
}

func TestLoadScoreWriter_MissingRequired(t *testing.T) {
	for key := range requiredScoreWriterEnv {
		t.Run(key, func(t *testing.T) {
			setScoreWriterEnv(t, map[string]string{key: ""})

			_, err := LoadScoreWriter()
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("err = %v, want one naming %s", err, key)
			}
		})
	}
}

func TestLoadScoreWriter_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"publish port zero", map[string]string{"SCORE_WRITER_PUBLISH_PORT": "0"}, "SCORE_WRITER_PUBLISH_PORT must be a port number"},
		{"internal port too high", map[string]string{"SCORE_WRITER_INTERNAL_PORT": "70000"}, "SCORE_WRITER_INTERNAL_PORT must be a port number"},
		{"database port negative", map[string]string{"SCORE_DB_PORT": "-1"}, "SCORE_DB_PORT must be a port number"},
		{"same ports", map[string]string{"SCORE_WRITER_PUBLISH_PORT": "9199"}, "must differ"},
		{"run-token JWKS not a URL", map[string]string{"EVAL_RUN_JWKS_URL": "amp-api/jwks.json"}, "EVAL_RUN_JWKS_URL must be an absolute http or https URL"},
		{"Thunder JWKS not http", map[string]string{"KEY_MANAGER_JWKS_URL": "file:///etc/jwks.json"}, "KEY_MANAGER_JWKS_URL must be an absolute http or https URL"},
		{"blank client ID", map[string]string{"SCORE_WRITER_INTERNAL_CLIENT_ID": " "}, "SCORE_WRITER_INTERNAL_CLIENT_ID is required"},
		{"only commas in issuer", map[string]string{"KEY_MANAGER_ISSUER": ", ,"}, "KEY_MANAGER_ISSUER is required"},
		{"retention under a day", map[string]string{"SCORE_RETENTION_DAYS": "0"}, "SCORE_RETENTION_DAYS must be at least 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setScoreWriterEnv(t, tt.env)

			_, err := LoadScoreWriter()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
