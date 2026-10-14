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

package core

import (
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	admissioncontrol "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/admission_control/v3"
	upstreamcodec "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/upstream_codec/v3"
	hcm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/http_connection_manager/v3"
	http "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	xdstype "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/util/protoconv"
	"istio.io/istio/pkg/util/grpc"
)

const (
	// admissionControlFilterName is the Envoy HTTP filter that performs
	// success-rate-based admission control. It is inserted into the upstream
	// HTTP filter chain of the destination cluster.
	admissionControlFilterName = "envoy.filters.http.admission_control"

	// upstreamCodecFilterName is the terminal upstream HTTP filter. Envoy
	// requires it to be the last filter in an upstream HTTP filter chain; it is
	// appended automatically so users never have to declare it.
	upstreamCodecFilterName = "envoy.filters.http.upstream_codec"
)

// applyAdmissionControlPolicy translates a DestinationRule admissionControl
// policy into an Envoy admission_control upstream HTTP filter on the cluster.
//
// Unlike outlierDetection, which maps to a field on the cluster proto,
// admission control is an upstream HTTP filter that lives inside
// HttpProtocolOptions.http_filters. This builder therefore extends the upstream
// filter chain rather than setting a cluster field, and it guarantees the
// terminal upstream_codec filter is appended last so the chain is valid:
//
//	admission_control -> upstream_codec -> network
//
// The already-selected UpstreamProtocolOptions (HTTP/1, HTTP/2 or auto) is left
// untouched; only http_filters is populated. This is the whole value over the
// hand-written EnvoyFilter workaround, which risks clobbering the protocol
// config or misordering the terminal codec.
func applyAdmissionControlPolicy(mc *clusterWrapper, acp *networking.AdmissionControlPolicy) {
	sr := acp.GetSuccessRate()
	if sr == nil {
		return
	}

	if mc.httpProtocolOptions == nil {
		mc.httpProtocolOptions = &http.HttpProtocolOptions{}
	}

	admissionControl := buildAdmissionControl(sr)

	// Insert admission_control first, then the terminal codec. The codec must
	// remain the last filter in the chain.
	mc.httpProtocolOptions.HttpFilters = []*hcm.HttpFilter{
		{
			Name: admissionControlFilterName,
			ConfigType: &hcm.HttpFilter_TypedConfig{
				TypedConfig: protoconv.MessageToAny(admissionControl),
			},
		},
		{
			Name: upstreamCodecFilterName,
			ConfigType: &hcm.HttpFilter_TypedConfig{
				TypedConfig: protoconv.MessageToAny(&upstreamcodec.UpstreamCodec{}),
			},
		},
	}
}

// buildAdmissionControl maps the Istio SuccessRate fields onto Envoy's
// AdmissionControl config. The Istio API uses behavior-named fields; each maps
// to a differently-named Envoy field (see the assignments below). Istio's
// scalar fields are wrapper types (google.protobuf.*Value): nil means unset, so
// Envoy applies its own default (samplingWindow=30s, sr_threshold=95%,
// aggression=1, rps_threshold=0, max_rejection_probability=80%); an explicit
// zero is distinguishable from unset and is honored as-is.
func buildAdmissionControl(sr *networking.SuccessRate) *admissioncontrol.AdmissionControl {
	ac := &admissioncontrol.AdmissionControl{
		// The evaluation_criteria oneof is required by Envoy, so it must always
		// be set. buildSuccessCriteria returns an empty SuccessCriteria when the
		// Istio field is unset, which selects Envoy's protocol defaults (HTTP:
		// 5xx is a failure; gRPC: standard success codes).
		EvaluationCriteria: &admissioncontrol.AdmissionControl_SuccessCriteria_{
			SuccessCriteria: buildSuccessCriteria(sr.GetSuccessCriteria()),
		},
	}

	if sr.SamplingWindow != nil {
		ac.SamplingWindow = sr.SamplingWindow
	}
	// threshold -> sr_threshold.default_value.value
	if sr.Threshold != nil {
		ac.SrThreshold = &core.RuntimePercent{
			DefaultValue: &xdstype.Percent{Value: sr.Threshold.GetValue()},
		}
	}
	// aggression -> aggression.default_value
	if sr.Aggression != nil {
		ac.Aggression = &core.RuntimeDouble{DefaultValue: sr.Aggression.GetValue()}
	}
	// minimumAttemptRate -> rps_threshold.default_value
	if sr.MinimumAttemptRate != nil {
		ac.RpsThreshold = &core.RuntimeUInt32{DefaultValue: sr.MinimumAttemptRate.GetValue()}
	}
	// maximumRejectionPercent -> max_rejection_probability.default_value.value
	if sr.MaximumRejectionPercent != nil {
		ac.MaxRejectionProbability = &core.RuntimePercent{
			DefaultValue: &xdstype.Percent{Value: sr.MaximumRejectionPercent.GetValue()},
		}
	}

	return ac
}

// buildSuccessCriteria maps the Istio SuccessCriteria onto Envoy's. A nil or
// empty Istio criteria yields an empty Envoy SuccessCriteria, which selects
// Envoy's protocol defaults (HTTP: statuses below 500 are successful; gRPC: the
// standard success codes). HTTP and gRPC are translated independently.
func buildSuccessCriteria(sc *networking.SuccessCriteria) *admissioncontrol.AdmissionControl_SuccessCriteria {
	out := &admissioncontrol.AdmissionControl_SuccessCriteria{}
	if sc == nil {
		return out
	}

	if h := sc.GetHttp(); h != nil {
		ranges := make([]*xdstype.Int32Range, 0, len(h.GetStatusRanges()))
		for _, r := range h.GetStatusRanges() {
			ranges = append(ranges, &xdstype.Int32Range{
				Start: int32(r.GetStart()),
				End:   int32(r.GetEnd()),
			})
		}
		out.HttpCriteria = &admissioncontrol.AdmissionControl_SuccessCriteria_HttpCriteria{
			HttpSuccessStatus: ranges,
		}
	}

	if g := sc.GetGrpc(); g != nil {
		statuses := make([]uint32, 0, len(g.GetStatusCodes()))
		for _, name := range g.GetStatusCodes() {
			code, ok := grpc.SupportedGRPCStatus[name]
			if !ok {
				// Defensive: config can reach istiod without having passed the
				// validating webhook (webhook absent, failurePolicy=Ignore, or a
				// non-Kubernetes config source). The webhook rejects unknown
				// names for the user; here we skip them rather than emit a
				// bogus code (the zero value would silently mean OK).
				continue
			}
			statuses = append(statuses, uint32(code))
		}
		out.GrpcCriteria = &admissioncontrol.AdmissionControl_SuccessCriteria_GrpcCriteria{
			GrpcSuccessStatus: statuses,
		}
	}

	return out
}
