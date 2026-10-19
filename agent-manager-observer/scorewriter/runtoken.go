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

package scorewriter

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wso2/agent-manager/agent-manager-observer/config"
)

const (
	runTokenLeeway    = 30 * time.Second
	jwksMinRefresh    = time.Minute
	jwksMaxBodyBytes  = 1 << 20
	jwksFetchTimeout  = 10 * time.Second
	maxRunClaimLength = 256
)

var (
	errUnknownKey      = errors.New("no key for the token's kid")
	errKeysUnavailable = errors.New("run-token keys unavailable")
)

// RunClaims are the claims of an evaluation run token.
type RunClaims struct {
	OuID            string `json:"ouId"`
	OuHandle        string `json:"ouHandle"`
	ProjectName     string `json:"projectName"`
	AgentName       string `json:"agentName"`
	EnvironmentName string `json:"environmentName"`
	MonitorID       string `json:"monitorId"`
	RunID           string `json:"runId"`
	jwt.RegisteredClaims
}

// runTokenVerifier verifies run tokens against agent-manager-service's run-token keys.
type runTokenVerifier struct {
	issuer   string
	audience string
	keys     *jwksCache
	now      func() time.Time
}

func newRunTokenVerifier(cfg config.RunTokenConfig, client *http.Client) *runTokenVerifier {
	return &runTokenVerifier{
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		keys:     newJWKSCache(cfg.JWKSURL, client, time.Now),
		now:      time.Now,
	}
}

// verify returns the claims of a valid run token, or why it is not one.
// errKeysUnavailable means the token could not be checked yet, not that it is invalid.
func (v *runTokenVerifier) verify(ctx context.Context, token string) (*RunClaims, error) {
	// Other token kinds are refused before any key fetch.
	unverified := &RunClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, unverified); err != nil {
		return nil, err
	}
	if unverified.Issuer != v.issuer || !slices.Contains(unverified.Audience, v.audience) {
		return nil, errors.New("not a run token: wrong iss or aud")
	}

	claims := &RunClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token has no kid")
		}
		return v.keys.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(runTokenLeeway),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return nil, err
	}
	if err := claims.check(); err != nil {
		return nil, err
	}
	return claims, nil
}

// check requires every run-token claim, beyond what the JWT library checks.
func (c *RunClaims) check() error {
	if c.NotBefore == nil {
		return errors.New("token has no nbf")
	}
	if c.IssuedAt == nil {
		return errors.New("token has no iat")
	}
	for _, f := range []struct{ name, value string }{
		{"ouId", c.OuID},
		{"ouHandle", c.OuHandle},
		{"projectName", c.ProjectName},
		{"agentName", c.AgentName},
		{"environmentName", c.EnvironmentName},
	} {
		if strings.TrimSpace(f.value) == "" || len(f.value) > maxRunClaimLength {
			return fmt.Errorf("token claim %s is missing or too long", f.name)
		}
	}
	if !isUUID(c.MonitorID) {
		return errors.New("token claim monitorId is not a UUID")
	}
	if !isUUID(c.RunID) {
		return errors.New("token claim runId is not a UUID")
	}
	if c.ID != c.RunID {
		return errors.New("token jti does not equal runId")
	}
	return nil
}

// jwksCache holds RSA keys by kid and fetches the set on an unknown kid at most once per minRefresh.
type jwksCache struct {
	url        string
	client     *http.Client
	now        func() time.Time
	minRefresh time.Duration

	mu   sync.RWMutex
	keys map[string]*rsa.PublicKey

	fetchMu   sync.Mutex
	lastFetch time.Time
	loaded    bool
}

func newJWKSCache(url string, client *http.Client, now func() time.Time) *jwksCache {
	return &jwksCache{url: url, client: client, now: now, minRefresh: jwksMinRefresh}
}

func (c *jwksCache) lookup(kid string) *rsa.PublicKey {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.keys[kid]
}

func (c *jwksCache) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if k := c.lookup(kid); k != nil {
		return k, nil
	}
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	if k := c.lookup(kid); k != nil {
		return k, nil
	}
	if !c.lastFetch.IsZero() && c.now().Sub(c.lastFetch) < c.minRefresh {
		if !c.loaded {
			return nil, errKeysUnavailable
		}
		return nil, errUnknownKey
	}
	c.lastFetch = c.now()
	keys, err := c.fetch(ctx)
	if err != nil {
		if !c.loaded {
			return nil, fmt.Errorf("%w: %w", errKeysUnavailable, err)
		}
		return nil, fmt.Errorf("refresh run-token keys: %w", err)
	}
	c.mu.Lock()
	c.keys = keys
	c.mu.Unlock()
	c.loaded = true
	if k := keys[kid]; k != nil {
		return k, nil
	}
	return nil, errUnknownKey
}

func (c *jwksCache) fetch(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, jwksFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			Alg string `json:"alg"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, jwksMaxBodyBytes)).Decode(&set); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" || (k.Use != "" && k.Use != "sig") || (k.Alg != "" && k.Alg != "RS256") {
			continue
		}
		pub, err := rsaKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	return keys, nil
}

func rsaKey(n, e string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(e)
	if err != nil {
		return nil, err
	}
	exp := new(big.Int).SetBytes(eb)
	if !exp.IsInt64() || exp.Int64() < 3 || exp.Int64() > 1<<31-1 {
		return nil, errors.New("bad exponent")
	}
	mod := new(big.Int).SetBytes(nb)
	if mod.BitLen() < 2048 {
		return nil, errors.New("modulus shorter than 2048 bits")
	}
	return &rsa.PublicKey{N: mod, E: int(exp.Int64())}, nil
}
