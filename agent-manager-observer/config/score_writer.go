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
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ScoreWriterConfig holds the score writer's configuration.
type ScoreWriterConfig struct {
	PublishPort  int
	InternalPort int
	TLS          TLSFiles
	Database     ScoreDatabaseConfig
	RunToken     RunTokenConfig
	InternalAuth InternalAuthConfig
	// RetentionDays bounds how old a published trace may be; it equals the score store's retention.
	RetentionDays int
	LogLevel      string
}

// TLSFiles are the PEM certificate and key a listener serves.
type TLSFiles struct {
	CertFile string
	KeyFile  string
}

// ScoreDatabaseConfig locates the score database and the writer's login.
type ScoreDatabaseConfig struct {
	Host         string
	Port         int
	Name         string
	User         string
	PasswordFile string
	CAFile       string
}

// RunTokenConfig verifies the evaluation run tokens agent-manager-service mints.
type RunTokenConfig struct {
	JWKSURL  string
	Issuer   string
	Audience string
}

// InternalAuthConfig verifies agent-manager-service's client-credentials token on the internal port.
type InternalAuthConfig struct {
	JWKSURL  string
	Issuer   []string
	ClientID string
}

// LoadScoreWriter loads the score writer's configuration from environment variables.
func LoadScoreWriter() (*ScoreWriterConfig, error) {
	cfg := &ScoreWriterConfig{
		PublishPort:  getEnvAsInt("SCORE_WRITER_PUBLISH_PORT", 9099),
		InternalPort: getEnvAsInt("SCORE_WRITER_INTERNAL_PORT", 9199),
		TLS: TLSFiles{
			CertFile: getEnv("SCORE_WRITER_TLS_CERT_FILE", ""),
			KeyFile:  getEnv("SCORE_WRITER_TLS_KEY_FILE", ""),
		},
		Database: ScoreDatabaseConfig{
			Host:         getEnv("SCORE_DB_HOST", ""),
			Port:         getEnvAsInt("SCORE_DB_PORT", 5432),
			Name:         getEnv("SCORE_DB_NAME", "amp_scores"),
			User:         getEnv("SCORE_DB_USER", "amp_scores_writer"),
			PasswordFile: getEnv("SCORE_DB_PASSWORD_FILE", ""),
			CAFile:       getEnv("SCORE_DB_CA_FILE", ""),
		},
		RunToken: RunTokenConfig{
			JWKSURL:  getEnv("EVAL_RUN_JWKS_URL", ""),
			Issuer:   getEnv("EVAL_RUN_ISSUER", "urn:wso2:amp:eval-run"),
			Audience: getEnv("EVAL_RUN_AUDIENCE", "urn:wso2:amp:eval-run"),
		},
		InternalAuth: InternalAuthConfig{
			JWKSURL:  getEnv("KEY_MANAGER_JWKS_URL", ""),
			Issuer:   getEnvAsOptionalList("KEY_MANAGER_ISSUER"),
			ClientID: getEnv("SCORE_WRITER_INTERNAL_CLIENT_ID", "amp-api-client"),
		},
		RetentionDays: getEnvAsInt("SCORE_RETENTION_DAYS", 30),
		LogLevel:      getEnv("LOG_LEVEL", "INFO"),
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *ScoreWriterConfig) validate() error {
	type setting[T any] struct {
		name  string
		value T
	}
	var errs []error
	for _, p := range []setting[int]{
		{"SCORE_WRITER_PUBLISH_PORT", c.PublishPort},
		{"SCORE_WRITER_INTERNAL_PORT", c.InternalPort},
		{"SCORE_DB_PORT", c.Database.Port},
	} {
		if p.value <= 0 || p.value > 65535 {
			errs = append(errs, fmt.Errorf("%s must be a port number, got %d", p.name, p.value))
		}
	}
	if c.PublishPort == c.InternalPort {
		errs = append(errs, fmt.Errorf("SCORE_WRITER_PUBLISH_PORT and SCORE_WRITER_INTERNAL_PORT must differ"))
	}
	for _, s := range []setting[string]{
		{"SCORE_WRITER_TLS_CERT_FILE", c.TLS.CertFile},
		{"SCORE_WRITER_TLS_KEY_FILE", c.TLS.KeyFile},
		{"SCORE_DB_HOST", c.Database.Host},
		{"SCORE_DB_NAME", c.Database.Name},
		{"SCORE_DB_USER", c.Database.User},
		{"SCORE_DB_PASSWORD_FILE", c.Database.PasswordFile},
		{"SCORE_DB_CA_FILE", c.Database.CAFile},
		{"EVAL_RUN_ISSUER", c.RunToken.Issuer},
		{"EVAL_RUN_AUDIENCE", c.RunToken.Audience},
		{"SCORE_WRITER_INTERNAL_CLIENT_ID", c.InternalAuth.ClientID},
	} {
		if strings.TrimSpace(s.value) == "" {
			errs = append(errs, fmt.Errorf("%s is required", s.name))
		}
	}
	for _, u := range []setting[string]{
		{"EVAL_RUN_JWKS_URL", c.RunToken.JWKSURL},
		{"KEY_MANAGER_JWKS_URL", c.InternalAuth.JWKSURL},
	} {
		if err := checkHTTPURL(u.value); err != nil {
			errs = append(errs, fmt.Errorf("%s %w", u.name, err))
		}
	}
	if c.RetentionDays < 1 {
		errs = append(errs, fmt.Errorf("SCORE_RETENTION_DAYS must be at least 1, got %d", c.RetentionDays))
	}
	if len(c.InternalAuth.Issuer) == 0 {
		errs = append(errs, fmt.Errorf("KEY_MANAGER_ISSUER is required"))
	}
	return errors.Join(errs...)
}

// checkHTTPURL requires an absolute http or https URL.
func checkHTTPURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http or https URL")
	}
	return nil
}
