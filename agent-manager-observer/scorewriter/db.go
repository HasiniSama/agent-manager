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
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wso2/agent-manager/agent-manager-observer/config"
)

// newPool builds the writer login's pool over verify-full TLS; it connects lazily.
func newPool(db config.ScoreDatabaseConfig) (*pgxpool.Pool, error) {
	if _, err := readPassword(db.PasswordFile); err != nil {
		return nil, err
	}
	dsn := url.URL{
		Scheme: "postgres",
		User:   url.User(db.User),
		Host:   net.JoinHostPort(db.Host, strconv.Itoa(db.Port)),
		Path:   "/" + db.Name,
		RawQuery: url.Values{
			"sslmode":         {"verify-full"},
			"sslrootcert":     {db.CAFile},
			"connect_timeout": {"5"},
		}.Encode(),
	}
	poolCfg, err := pgxpool.ParseConfig(dsn.String())
	if err != nil {
		return nil, fmt.Errorf("score database config: %w", err)
	}
	// Read on every new connection, so a rotated password needs no restart.
	poolCfg.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
		password, err := readPassword(db.PasswordFile)
		if err != nil {
			return err
		}
		cc.Password = password
		return nil
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("score database pool: %w", err)
	}
	return pool, nil
}

func readPassword(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the score database password: %w", err)
	}
	password := strings.TrimRight(string(b), "\r\n")
	if password == "" {
		return "", fmt.Errorf("the score database password file %s is empty", path)
	}
	return password, nil
}
