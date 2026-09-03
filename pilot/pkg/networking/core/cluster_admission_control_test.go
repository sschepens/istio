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
	"testing"

	cluster "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	admissioncontrol "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/admission_control/v3"
	http "github.com/envoyproxy/go-control-plane/envoy/extensions/upstreams/http/v3"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	v3 "istio.io/istio/pilot/pkg/xds/v3"
	"istio.io/istio/pilot/test/xdstest"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/protocol"
)

// admissionControlWithSuccessRate wraps a SuccessRate in the
// AdmissionControlPolicy oneof, as every DestinationRule construction below needs.
func admissionControlWithSuccessRate(sr *networking.SuccessRate) *networking.AdmissionControlPolicy {
	return &networking.AdmissionControlPolicy{
		Strategy: &networking.AdmissionControlPolicy_SuccessRate{SuccessRate: sr},
	}
}

// extractHTTPProtocolOptions pulls the HttpProtocolOptions extension out of a
// generated cluster, or nil if none is present.
func extractHTTPProtocolOptions(g *WithT, c *cluster.Cluster) *http.HttpProtocolOptions {
	if c.TypedExtensionProtocolOptions == nil {
		return nil
	}
	anyOptions := c.TypedExtensionProtocolOptions[v3.HttpProtocolOptionsType]
	if anyOptions == nil {
		return nil
	}
	opts := &http.HttpProtocolOptions{}
	g.Expect(anyOptions.UnmarshalTo(opts)).To(Succeed())
	return opts
}

// assertTerminalCodecChain verifies the upstream HTTP filter chain is exactly
// [admission_control, upstream_codec] with the codec last. This is the central
// invariant of the feature: the terminal upstream_codec is appended
// automatically even though it appears in no DestinationRule.
func assertTerminalCodecChain(g *WithT, opts *http.HttpProtocolOptions) {
	g.Expect(opts).NotTo(BeNil())
	filters := opts.GetHttpFilters()
	g.Expect(filters).To(HaveLen(2), "expected admission_control + terminal upstream_codec")
	g.Expect(filters[0].GetName()).To(Equal(admissionControlFilterName))
	g.Expect(filters[1].GetName()).To(Equal(upstreamCodecFilterName),
		"upstream_codec must be the terminal (last) upstream filter")
}

// TestAdmissionControlPolicyHappyPath is case 1: a registered host with an
// admissionControl policy produces a cluster carrying the admission_control
// filter followed by the auto-added terminal upstream_codec, and the
// AdmissionControl config reflects the SuccessRate fields.
func TestAdmissionControlPolicyHappyPath(t *testing.T) {
	g := NewWithT(t)

	c := xdstest.ExtractCluster("outbound|8080||*.example.org",
		buildTestClusters(clusterTest{
			t: t, serviceHostname: "*.example.org", serviceResolution: model.DNSLB, nodeType: model.SidecarProxy,
			locality: &core.Locality{}, mesh: testMesh(),
			destRule: &networking.DestinationRule{
				Host: "*.example.org",
				TrafficPolicy: &networking.TrafficPolicy{
					AdmissionControl: admissionControlWithSuccessRate(&networking.SuccessRate{
						SamplingWindow:          durationpb.New(30 * 1e9),
						Threshold:               wrapperspb.Double(95),
						Aggression:              wrapperspb.Double(2),
						MinimumAttemptRate:      wrapperspb.UInt32(10),
						MaximumRejectionPercent: wrapperspb.Double(80),
					}),
				},
			},
		}))

	opts := extractHTTPProtocolOptions(g, c)
	assertTerminalCodecChain(g, opts)

	// The admission_control typed config should carry the mapped SuccessRate values.
	ac := &admissioncontrol.AdmissionControl{}
	g.Expect(opts.GetHttpFilters()[0].GetTypedConfig().UnmarshalTo(ac)).To(Succeed())
	g.Expect(ac.GetSrThreshold().GetDefaultValue().GetValue()).To(Equal(float64(95)))
	g.Expect(ac.GetAggression().GetDefaultValue()).To(Equal(float64(2)))
	g.Expect(ac.GetRpsThreshold().GetDefaultValue()).To(Equal(uint32(10)))
	g.Expect(ac.GetMaxRejectionProbability().GetDefaultValue().GetValue()).To(Equal(float64(80)))
	g.Expect(ac.GetSamplingWindow().GetSeconds()).To(Equal(int64(30)))
	// The required evaluation_criteria oneof must be present (empty SuccessCriteria
	// selects Envoy's default HTTP/gRPC success criteria).
	g.Expect(ac.GetSuccessCriteria()).NotTo(BeNil())
}

// TestAdmissionControlPolicyCoexistsWithOutlierDetection is case 3: with
// outlierDetection set in parallel, the admission_control filter is injected
// without disturbing the cluster's outlier_detection field.
func TestAdmissionControlPolicyCoexistsWithOutlierDetection(t *testing.T) {
	g := NewWithT(t)

	c := xdstest.ExtractCluster("outbound|8080||*.example.org",
		buildTestClusters(clusterTest{
			t: t, serviceHostname: "*.example.org", serviceResolution: model.DNSLB, nodeType: model.SidecarProxy,
			locality: &core.Locality{}, mesh: testMesh(),
			destRule: &networking.DestinationRule{
				Host: "*.example.org",
				TrafficPolicy: &networking.TrafficPolicy{
					OutlierDetection: &networking.OutlierDetection{
						ConsecutiveGatewayErrors: &wrapperspb.UInt32Value{Value: 3},
					},
					AdmissionControl: admissionControlWithSuccessRate(&networking.SuccessRate{
						Threshold: wrapperspb.Double(90),
					}),
				},
			},
		}))

	// outlier_detection cluster field is untouched.
	g.Expect(c.OutlierDetection).NotTo(BeNil())
	g.Expect(c.OutlierDetection.ConsecutiveGatewayFailure.GetValue()).To(Equal(uint32(3)))

	// admission_control filter chain is present and terminal-codec correct.
	opts := extractHTTPProtocolOptions(g, c)
	assertTerminalCodecChain(g, opts)
}

// TestAdmissionControlPolicyAbsent is the regression guard (case 4): no policy
// means the cluster has no upstream HTTP filter chain, identical to today.
func TestAdmissionControlPolicyAbsent(t *testing.T) {
	g := NewWithT(t)

	c := xdstest.ExtractCluster("outbound|8080||*.example.org",
		buildTestClusters(clusterTest{
			t: t, serviceHostname: "*.example.org", serviceResolution: model.DNSLB, nodeType: model.SidecarProxy,
			locality: &core.Locality{}, mesh: testMesh(),
			destRule: &networking.DestinationRule{
				Host:          "*.example.org",
				TrafficPolicy: &networking.TrafficPolicy{},
			},
		}))

	opts := extractHTTPProtocolOptions(g, c)
	if opts != nil {
		g.Expect(opts.GetHttpFilters()).To(BeEmpty(), "no policy must not inject any upstream filters")
	}
}

// TestAdmissionControlPolicyPreservesProtocol is the protocol case: applying the
// policy to a cluster whose protocol istiod already chose (HTTP/2) must NOT
// overwrite http2_protocol_options — it only appends the filter chain. This is
// the most important bug the native generation exists to prevent.
func TestAdmissionControlPolicyPreservesProtocol(t *testing.T) {
	g := NewWithT(t)

	// Simulate istiod having already selected explicit HTTP/2 for this cluster,
	// as the connection-pool / h2-upgrade path would.
	mc := newClusterWrapper(&cluster.Cluster{Name: "outbound|8080||h2.example.org"})
	mc.httpProtocolOptions = &http.HttpProtocolOptions{
		UpstreamProtocolOptions: &http.HttpProtocolOptions_ExplicitHttpConfig_{
			ExplicitHttpConfig: &http.HttpProtocolOptions_ExplicitHttpConfig{
				ProtocolConfig: &http.HttpProtocolOptions_ExplicitHttpConfig_Http2ProtocolOptions{
					Http2ProtocolOptions: &core.Http2ProtocolOptions{},
				},
			},
		},
	}

	applyAdmissionControlPolicy(mc, admissionControlWithSuccessRate(&networking.SuccessRate{
		Threshold: wrapperspb.Double(95),
	}))

	built := mc.build()
	opts := extractHTTPProtocolOptions(g, built)
	assertTerminalCodecChain(g, opts)

	// The HTTP/2 protocol config selected earlier survives untouched.
	explicit := opts.GetExplicitHttpConfig()
	g.Expect(explicit).NotTo(BeNil())
	g.Expect(explicit.GetHttp2ProtocolOptions()).NotTo(BeNil(),
		"http2_protocol_options must be preserved, not downgraded to HTTP/1")
	g.Expect(explicit.GetHttpProtocolOptions()).To(BeNil(),
		"cluster must not have been silently downgraded to HTTP/1")
}

// extractAdmissionControl pulls the admission_control typed config out of the
// first upstream HTTP filter of a cluster. It fails the test if the chain is
// not present.
func extractAdmissionControl(g *WithT, c *cluster.Cluster) *admissioncontrol.AdmissionControl {
	opts := extractHTTPProtocolOptions(g, c)
	assertTerminalCodecChain(g, opts)
	ac := &admissioncontrol.AdmissionControl{}
	g.Expect(opts.GetHttpFilters()[0].GetTypedConfig().UnmarshalTo(ac)).To(Succeed())
	return ac
}

func buildWaypointAdmissionControlCluster(
	t testing.TB,
	subset string,
	portProtocol protocol.Instance,
	policy *networking.TrafficPolicy,
) *cluster.Cluster {
	port := &model.Port{Name: "http", Port: 8080, Protocol: portProtocol}
	service := &model.Service{
		Hostname:   host.Name("reviews.default.svc.cluster.local"),
		Ports:      model.PortList{port},
		Resolution: model.ClientSideLB,
		Attributes: model.ServiceAttributes{
			Name:      "reviews",
			Namespace: "default",
		},
	}
	cg := NewConfigGenTest(t, TestOptions{
		Services:   []*model.Service{service},
		MeshConfig: testMesh(),
	})
	proxy := cg.SetupProxy(&model.Proxy{Type: model.Waypoint})
	cb := NewClusterBuilder(proxy, &model.PushRequest{Push: cg.PushContext()}, model.DisabledCache{})

	return cb.buildWaypointInboundVIPCluster(
		proxy,
		service,
		*port,
		subset,
		cg.PushContext().Mesh,
		policy,
		&config.Config{},
	)
}

// TestAdmissionControlPolicyWaypointCluster verifies the waypoint semantics:
// admission control is attached to the inbound VIP HTTP cluster, so all callers
// routed through that service/port/subset cluster share the same success-rate
// history and rejection decisions.
func TestAdmissionControlPolicyWaypointCluster(t *testing.T) {
	g := NewWithT(t)
	policy := &networking.TrafficPolicy{
		AdmissionControl: admissionControlWithSuccessRate(&networking.SuccessRate{
			Threshold: wrapperspb.Double(90),
		}),
	}

	c := buildWaypointAdmissionControlCluster(t, "http", protocol.HTTP, policy)
	g.Expect(c.Name).To(Equal("inbound-vip|8080|http|reviews.default.svc.cluster.local"))
	ac := extractAdmissionControl(g, c)
	g.Expect(ac.GetSrThreshold().GetDefaultValue().GetValue()).To(Equal(float64(90)))
}

func TestAdmissionControlPolicyWaypointAbsent(t *testing.T) {
	g := NewWithT(t)
	c := buildWaypointAdmissionControlCluster(t, "http", protocol.HTTP, &networking.TrafficPolicy{})

	opts := extractHTTPProtocolOptions(g, c)
	if opts != nil {
		g.Expect(opts.GetHttpFilters()).To(BeEmpty(), "waypoint cluster without policy must not get upstream filters")
	}
}

// TestAdmissionControlPolicyWaypointSkipsTCP ensures an HTTP admission-control
// filter is not injected into the TCP sibling generated for an AUTO port.
func TestAdmissionControlPolicyWaypointSkipsTCP(t *testing.T) {
	g := NewWithT(t)
	policy := &networking.TrafficPolicy{
		AdmissionControl: admissionControlWithSuccessRate(&networking.SuccessRate{
			Threshold: wrapperspb.Double(90),
		}),
	}

	c := buildWaypointAdmissionControlCluster(t, "tcp", protocol.Unsupported, policy)
	g.Expect(c.Name).To(Equal("inbound-vip|8080|tcp|reviews.default.svc.cluster.local"))
	opts := extractHTTPProtocolOptions(g, c)
	if opts != nil {
		g.Expect(opts.GetHttpFilters()).To(BeEmpty(), "TCP waypoint cluster must not get upstream HTTP filters")
	}
}

// TestAdmissionControlPolicyPortLevel is the port-level case (RFC verification
// case 2): the policy declared under portLevelSettings is applied only to the
// selected port's cluster, proving the field is honored on PortTrafficPolicy and
// that a non-matching port is left untouched.
func TestAdmissionControlPolicyPortLevel(t *testing.T) {
	g := NewWithT(t)

	// The test service exposes port 8080 (HTTP). Declaring the policy on 8080
	// injects the chain into its cluster.
	matched := xdstest.ExtractCluster("outbound|8080||*.example.org",
		buildTestClusters(clusterTest{
			t: t, serviceHostname: "*.example.org", serviceResolution: model.DNSLB, nodeType: model.SidecarProxy,
			locality: &core.Locality{}, mesh: testMesh(),
			destRule: &networking.DestinationRule{
				Host: "*.example.org",
				TrafficPolicy: &networking.TrafficPolicy{
					PortLevelSettings: []*networking.TrafficPolicy_PortTrafficPolicy{
						{
							Port: &networking.PortSelector{Number: 8080},
							AdmissionControl: admissionControlWithSuccessRate(&networking.SuccessRate{
								Threshold: wrapperspb.Double(90),
							}),
						},
					},
				},
			},
		}))

	ac := extractAdmissionControl(g, matched)
	g.Expect(ac.GetSrThreshold().GetDefaultValue().GetValue()).To(Equal(float64(90)))

	// Declaring the policy on a different port (9090) must NOT inject the chain
	// into the 8080 cluster: port-level settings are scoped to their port.
	notMatched := xdstest.ExtractCluster("outbound|8080||*.example.org",
		buildTestClusters(clusterTest{
			t: t, serviceHostname: "*.example.org", serviceResolution: model.DNSLB, nodeType: model.SidecarProxy,
			locality: &core.Locality{}, mesh: testMesh(),
			destRule: &networking.DestinationRule{
				Host: "*.example.org",
				TrafficPolicy: &networking.TrafficPolicy{
					PortLevelSettings: []*networking.TrafficPolicy_PortTrafficPolicy{
						{
							Port: &networking.PortSelector{Number: 9090},
							AdmissionControl: admissionControlWithSuccessRate(&networking.SuccessRate{
								Threshold: wrapperspb.Double(90),
							}),
						},
					},
				},
			},
		}))

	opts := extractHTTPProtocolOptions(g, notMatched)
	if opts != nil {
		g.Expect(opts.GetHttpFilters()).To(BeEmpty(),
			"a port-level policy on another port must not touch the 8080 cluster")
	}
}

// TestAdmissionControlPolicySubsetInheritance is the subset-inheritance case (RFC
// case 3, part A): a subset that does not override trafficPolicy inherits the
// top-level admissionControl, so both the base and the subset cluster carry
// the filter chain.
func TestAdmissionControlPolicySubsetInheritance(t *testing.T) {
	g := NewWithT(t)

	clusters := buildTestClusters(clusterTest{
		t: t, serviceHostname: "*.example.org", serviceResolution: model.DNSLB, nodeType: model.SidecarProxy,
		locality: &core.Locality{}, mesh: testMesh(),
		destRule: &networking.DestinationRule{
			Host: "*.example.org",
			TrafficPolicy: &networking.TrafficPolicy{
				AdmissionControl: admissionControlWithSuccessRate(&networking.SuccessRate{
					Threshold: wrapperspb.Double(95),
				}),
			},
			Subsets: []*networking.Subset{
				{Name: "v1"}, // no trafficPolicy: inherits the top-level policy
			},
		},
	})

	base := xdstest.ExtractCluster("outbound|8080||*.example.org", clusters)
	subset := xdstest.ExtractCluster("outbound|8080|v1|*.example.org", clusters)

	baseAC := extractAdmissionControl(g, base)
	g.Expect(baseAC.GetSrThreshold().GetDefaultValue().GetValue()).To(Equal(float64(95)))

	subsetAC := extractAdmissionControl(g, subset)
	g.Expect(subsetAC.GetSrThreshold().GetDefaultValue().GetValue()).To(Equal(float64(95)),
		"a subset with no override must inherit the top-level policy")
}

// TestAdmissionControlPolicySubsetOverride is the subset-override case (RFC case 3,
// part B): a subset that sets its own admissionControl overrides the
// top-level policy for its cluster while the base cluster keeps the top-level
// value.
func TestAdmissionControlPolicySubsetOverride(t *testing.T) {
	g := NewWithT(t)

	clusters := buildTestClusters(clusterTest{
		t: t, serviceHostname: "*.example.org", serviceResolution: model.DNSLB, nodeType: model.SidecarProxy,
		locality: &core.Locality{}, mesh: testMesh(),
		destRule: &networking.DestinationRule{
			Host: "*.example.org",
			TrafficPolicy: &networking.TrafficPolicy{
				AdmissionControl: admissionControlWithSuccessRate(&networking.SuccessRate{
					Threshold: wrapperspb.Double(95),
				}),
			},
			Subsets: []*networking.Subset{
				{
					Name: "v1",
					TrafficPolicy: &networking.TrafficPolicy{
						AdmissionControl: admissionControlWithSuccessRate(&networking.SuccessRate{
							Threshold: wrapperspb.Double(80),
						}),
					},
				},
			},
		},
	})

	base := xdstest.ExtractCluster("outbound|8080||*.example.org", clusters)
	subset := xdstest.ExtractCluster("outbound|8080|v1|*.example.org", clusters)

	baseAC := extractAdmissionControl(g, base)
	g.Expect(baseAC.GetSrThreshold().GetDefaultValue().GetValue()).To(Equal(float64(95)),
		"the base cluster keeps the top-level threshold")

	subsetAC := extractAdmissionControl(g, subset)
	g.Expect(subsetAC.GetSrThreshold().GetDefaultValue().GetValue()).To(Equal(float64(80)),
		"the subset overrides the top-level threshold")
}
