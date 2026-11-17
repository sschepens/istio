// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package options

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"istio.io/istio/pkg/test/util/assert"
)

func TestTrafficExtensionEnvironment(t *testing.T) {
	// These values are read at package initialization, so each scenario needs a fresh process.
	const childCase = "ISTIO_TEST_TRAFFIC_EXTENSION_ENV_CASE"
	suffixes := []string{"INSECURE_REGISTRIES", "MODULE_EXPIRY", "PURGE_INTERVAL", "HTTP_REQUEST_TIMEOUT", "HTTP_REQUEST_MAX_RETRIES"}
	legacy := []string{"legacy:5000", "12h", "30m", "10s", "3"}
	modern := []string{"modern:5000", "48h", "2h", "20s", "7"}
	for _, tc := range []struct {
		name           string
		legacy, modern []string
		want           []string
	}{
		{name: "defaults", want: []string{"", "24h0m0s", "1h0m0s", "15s", "5"}},
		{name: "legacy fallback", legacy: legacy, want: []string{"legacy:5000", "12h0m0s", "30m0s", "10s", "3"}},
		{name: "new variables", modern: modern, want: []string{"modern:5000", "48h0m0s", "2h0m0s", "20s", "7"}},
		{name: "new overrides legacy", legacy: legacy, modern: modern, want: []string{"modern:5000", "48h0m0s", "2h0m0s", "20s", "7"}},
		{name: "explicit empty and zero override legacy", legacy: legacy, modern: []string{"", "0s", "0s", "0s", "0"}, want: []string{"", "0s", "0s", "0s", "0"}},
	} {
		if scenario := os.Getenv(childCase); scenario != "" {
			if scenario != tc.name {
				continue
			}
			got := []string{trafficExtensionInsecureRegistries, trafficExtensionModuleExpiry.String(),
				trafficExtensionPurgeInterval.String(), trafficExtensionHTTPRequestTimeout.String(), strconv.Itoa(trafficExtensionHTTPRequestMaxRetries)}
			assert.Equal(t, got, tc.want)
			return
		}
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestTrafficExtensionEnvironment$")
			for _, entry := range os.Environ() {
				name, _, _ := strings.Cut(entry, "=")
				skip := name == childCase
				for _, suffix := range suffixes {
					skip = skip || name == "WASM_"+suffix || name == "TRAFFIC_EXTENSION_"+suffix
				}
				if !skip {
					cmd.Env = append(cmd.Env, entry)
				}
			}
			cmd.Env = append(cmd.Env, childCase+"="+tc.name)
			for i, suffix := range suffixes {
				if tc.legacy != nil {
					cmd.Env = append(cmd.Env, "WASM_"+suffix+"="+tc.legacy[i])
				}
				if tc.modern != nil {
					cmd.Env = append(cmd.Env, "TRAFFIC_EXTENSION_"+suffix+"="+tc.modern[i])
				}
			}
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v\n%s", err, output)
			}
		})
	}
	if scenario := os.Getenv(childCase); scenario != "" {
		t.Fatalf("unknown scenario %q", scenario)
	}
}
