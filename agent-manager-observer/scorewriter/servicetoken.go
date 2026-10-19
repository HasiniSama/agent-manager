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
	"cmp"
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/wso2/agent-manager/agent-manager-observer/config"
)

const serviceTokenLeeway = 30 * time.Second

var errWrongClient = errors.New("token belongs to another client")

// serviceClaims are the claims read from a Thunder client-credentials token.
type serviceClaims struct {
	ClientID string `json:"client_id"`
	jwt.RegisteredClaims
}

// serviceTokenVerifier admits agent-manager-service's client-credentials token by issuer, audience and client ID; scopes are not read.
type serviceTokenVerifier struct {
	issuers  []string
	audience string
	clientID string
	keys     *jwksCache
	now      func() time.Time
}

func newServiceTokenVerifier(cfg config.InternalAuthConfig, client *http.Client) *serviceTokenVerifier {
	return &serviceTokenVerifier{
		issuers:  cfg.Issuer,
		audience: cmp.Or(cfg.Audience, config.DefaultScoreWriterAudience),
		clientID: cfg.ClientID,
		keys:     newJWKSCache(cfg.JWKSURL, client, time.Now),
		now:      time.Now,
	}
}

// verify returns the claims of agent-manager-service's token; errWrongClient comes with the other client's claims.
// errKeysUnavailable means the token could not be checked yet, not that it is invalid.
func (v *serviceTokenVerifier) verify(ctx context.Context, token string) (*serviceClaims, error) {
	// Tokens from another issuer or for another audience, run tokens among them, are refused before any key fetch.
	unverified := &serviceClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, unverified); err != nil {
		return nil, err
	}
	if !slices.Contains(v.issuers, unverified.Issuer) || !slices.Contains(unverified.Audience, v.audience) {
		return nil, errors.New("not a writer token: wrong iss or aud")
	}

	claims := &serviceClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token has no kid")
		}
		return v.keys.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(serviceTokenLeeway),
		jwt.WithTimeFunc(v.now),
	)
	if err != nil {
		return nil, err
	}
	if claims.ClientID != v.clientID {
		return claims, errWrongClient
	}
	return claims, nil
}
