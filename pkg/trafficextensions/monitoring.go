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

package trafficextensions

import "istio.io/istio/pkg/monitoring"

// Const strings for label value.
const (
	wasmExtensionType          = "wasm"
	dynamicModuleExtensionType = "dynamic_module"

	// For remote fetch metric.
	fetchSuccess     = "success"
	downloadFailure  = "download_failure"
	manifestFailure  = "manifest_failure"
	checksumMismatch = "checksum_mismatched"

	// For extension conversion metrics.
	conversionSuccess   = "success"
	noRemoteLoad        = "no_remote_load"
	marshalFailure      = "marshal_failure"
	unmarshalFailure    = "unmarshal_failure"
	fetchFailure        = "fetch_failure"
	missRemoteFetchHint = "miss_remote_fetch_hint"
)

var (
	extensionTypeTag = monitoring.CreateLabel("extension_type")
	hitTag           = monitoring.CreateLabel("hit")
	resultTag        = monitoring.CreateLabel("result")

	trafficExtensionCacheEntries = monitoring.NewGauge(
		"traffic_extension_cache_entries",
		"number of remote traffic extension cache entries.",
	)

	trafficExtensionCacheLookupCount = monitoring.NewSum(
		"traffic_extension_cache_lookup_count",
		"number of remote traffic extension cache lookups.",
	)

	trafficExtensionRemoteFetchCount = monitoring.NewSum(
		"traffic_extension_remote_fetch_count",
		"number of remote traffic extension fetches and results, including success, download failure, and checksum mismatch.",
	)

	trafficExtensionConfigConversionCount = monitoring.NewSum(
		"traffic_extension_config_conversion_count",
		"number of Wasm and DynamicModule config conversions and results, including success, no remote load, unmarshal failure, marshal failure, remote fetch failure, and missing remote fetch hints.",
	)

	trafficExtensionConfigConversionDuration = monitoring.NewDistribution(
		"traffic_extension_config_conversion_duration",
		"Time in milliseconds istio-agent spends converting each traffic extension config.",
		[]float64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048, 4096, 8192, 16384},
	)
)

func extensionTypeForModule(dynamicModule bool) string {
	if dynamicModule {
		return dynamicModuleExtensionType
	}
	return wasmExtensionType
}
