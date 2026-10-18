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

package scorestore

import (
	"os"
	"path/filepath"
	"testing"
)

// chartMigrationsDir holds the copy the observability chart's migration Job applies.
const chartMigrationsDir = "../../deployments/helm-charts/wso2-amp-observability-extension/files/score-store/migrations"

func TestMigrationsMatchChart(t *testing.T) {
	local := readSQLFiles(t, "migrations")
	chart := readSQLFiles(t, chartMigrationsDir)
	if len(local) == 0 {
		t.Fatal("no SQL files in scorestore/migrations")
	}
	for name, body := range chart {
		got, ok := local[name]
		if !ok {
			t.Errorf("%s is in the chart but not in scorestore/migrations", name)
			continue
		}
		if got != body {
			t.Errorf("%s differs between the chart and scorestore/migrations", name)
		}
	}
	for name := range local {
		if _, ok := chart[name]; !ok {
			t.Errorf("%s is in scorestore/migrations but not in the chart", name)
		}
	}
}

// readSQLFiles maps each .sql file name in dir to its contents.
func readSQLFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	files := make(map[string]string, len(paths))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		files[filepath.Base(path)] = string(body)
	}
	return files
}
