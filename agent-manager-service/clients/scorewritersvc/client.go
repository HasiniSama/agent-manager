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

// Package scorewritersvc calls the score writer's internal routes.
package scorewritersvc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	occlient "github.com/wso2/agent-manager/agent-manager-service/clients/openchoreosvc/client"
	"github.com/wso2/agent-manager/agent-manager-service/clients/requests"
)

const deletePath = "/internal/v1/scores/delete"

//go:generate moq -rm -fmt goimports -skip-ensure -pkg clientmocks -out ../clientmocks/score_writer_client_fake.go . ScoreWriterClient:ScoreWriterClientMock

// ScoreWriterClient sends score deletes to the score writer.
type ScoreWriterClient interface {
	DeleteScores(ctx context.Context, req DeleteScoresRequest) error
}

// DeleteScoresRequest is the body of POST /internal/v1/scores/delete.
type DeleteScoresRequest struct {
	OrgID            string     `json:"orgId"`
	MonitorIDs       []string   `json:"monitorIds"`
	EvaluatorName    string     `json:"evaluatorName,omitempty"`
	RunStartedBefore *time.Time `json:"runStartedBefore,omitempty"`
}

// Config contains configuration for the score writer client.
type Config struct {
	// BaseURL is the writer's internal https base URL.
	BaseURL string
	// CAFile is the PEM bundle that verifies the writer; empty uses the system pool.
	CAFile       string
	Timeout      time.Duration
	AuthProvider occlient.AuthProvider
}

type scoreWriterClient struct {
	baseURL      string
	httpClient   requests.HttpClient
	authProvider occlient.AuthProvider
}

// NewScoreWriterClient creates a score writer client.
func NewScoreWriterClient(cfg Config) (ScoreWriterClient, error) {
	if !strings.HasPrefix(cfg.BaseURL, "https://") {
		return nil, fmt.Errorf("score writer URL must use https")
	}
	if cfg.AuthProvider == nil {
		return nil, fmt.Errorf("auth provider is required")
	}
	if cfg.Timeout <= 0 {
		return nil, fmt.Errorf("timeout must be positive")
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read score writer CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("score writer CA %s holds no PEM certificate", cfg.CAFile)
		}
		tlsConfig.RootCAs = pool
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &scoreWriterClient{
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		httpClient:   &http.Client{Timeout: cfg.Timeout, Transport: transport},
		authProvider: cfg.AuthProvider,
	}, nil
}

// DeleteScores asks the writer to delete scores, retrying once with a fresh token on a 401.
func (c *scoreWriterClient) DeleteScores(ctx context.Context, body DeleteScoresRequest) error {
	err := c.post(ctx, body)
	var httpErr *requests.HttpError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusUnauthorized {
		c.authProvider.InvalidateToken()
		err = c.post(ctx, body)
	}
	if err != nil {
		return fmt.Errorf("scorewritersvc.DeleteScores: %w", err)
	}
	return nil
}

func (c *scoreWriterClient) post(ctx context.Context, body DeleteScoresRequest) error {
	token, err := c.authProvider.GetToken(ctx)
	if err != nil {
		return fmt.Errorf("failed to get auth token: %w", err)
	}
	req := &requests.HttpRequest{
		Name:   "scorewritersvc.DeleteScores",
		URL:    c.baseURL + deletePath,
		Method: http.MethodPost,
	}
	req.SetHeader("Authorization", "Bearer "+token)
	req.SetJson(body)

	result := requests.SendRequest(ctx, c.httpClient, req)
	if err := result.Err(); err != nil {
		return err
	}
	if code := result.StatusCode(); code < 200 || code > 299 {
		return &requests.HttpError{StatusCode: code}
	}
	return nil
}

// IsRejected reports whether the writer refused the request itself, so resending it cannot succeed.
// Authorization failures, timeouts and throttling are not rejections: they clear without the row changing.
func IsRejected(err error) bool {
	var httpErr *requests.HttpError
	if !errors.As(err, &httpErr) || httpErr.StatusCode < 400 || httpErr.StatusCode > 499 {
		return false
	}
	switch httpErr.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return true
}
