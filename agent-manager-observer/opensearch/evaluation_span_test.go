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

package opensearch

import "testing"

// Only a non-empty string run ID makes an evaluation span.
func TestIsEvaluationSpan(t *testing.T) {
	cases := []struct {
		name  string
		attrs map[string]interface{}
		want  bool
	}{
		{name: "run ID", attrs: map[string]interface{}{EvalRunIDAttribute: "run-1"}, want: true},
		{name: "empty run ID", attrs: map[string]interface{}{EvalRunIDAttribute: ""}},
		{name: "run ID not a string", attrs: map[string]interface{}{EvalRunIDAttribute: float64(1)}},
		{name: "evaluation result without run ID", attrs: map[string]interface{}{
			"gen_ai.evaluation.name": "Relevance", "gen_ai.evaluation.score.value": 0.9}},
		{name: "no attributes"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsEvaluationSpan(tc.attrs); got != tc.want {
				t.Errorf("IsEvaluationSpan = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestMayBeEvaluationSpanName(t *testing.T) {
	cases := map[string]bool{
		"evaluation Hallucination": true,
		" Evaluation tool.chat ":   true,
		"evaluation":               true,
		"evaluations":              false,
		"run evaluation":           false,
		"invoke_agent":             false,
		"":                         false,
	}
	for name, want := range cases {
		if got := MayBeEvaluationSpanName(name); got != want {
			t.Errorf("MayBeEvaluationSpanName(%q) = %t, want %t", name, got, want)
		}
	}
}
