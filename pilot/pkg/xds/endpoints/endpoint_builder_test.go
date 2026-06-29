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

package endpoints

import (
	"fmt"
	"reflect"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"istio.io/api/label"
	meshconfig "istio.io/api/mesh/v1alpha1"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/memory"
	"istio.io/istio/pilot/pkg/serviceregistry/util/xdsfake"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/workloadapi"
)

// mockAmbientIndex is a test implementation of AmbientIndexes that returns mock service info
type mockAmbientIndex struct {
	model.NoopAmbientIndexes
	serviceInfos []*model.ServiceInfo
}

func (m *mockAmbientIndex) ServiceInfo(key string) *model.ServiceInfo {
	for _, info := range m.serviceInfos {
		svcKey := fmt.Sprintf("%s/%s", info.GetNamespace(), info.Service.GetHostname())
		if key == svcKey {
			return info
		}
	}
	return nil
}

// MockDiscovery is an in-memory ServiceDiscover with mock services
type localServiceDiscovery struct {
	services         []*model.Service
	serviceInstances []*model.ServiceInstance

	model.NetworkGatewaysHandler
}

var _ model.ServiceDiscovery = &localServiceDiscovery{}

func (l *localServiceDiscovery) Services() []*model.Service {
	return l.services
}

func (l *localServiceDiscovery) GetService(host.Name) *model.Service {
	panic("implement me")
}

func (l *localServiceDiscovery) GetProxyServiceTargets(*model.Proxy) []model.ServiceTarget {
	var svcTS []model.ServiceTarget
	for _, svc := range l.services {
		var svcT model.ServiceTarget
		svcT.Service = svc
		svcTS = append(svcTS, svcT)
	}
	return svcTS
}

func (l *localServiceDiscovery) GetProxyWorkloadLabels(*model.Proxy) labels.Instance {
	panic("implement me")
}

func (l *localServiceDiscovery) GetIstioServiceAccounts(*model.Service) []string {
	return nil
}

func (l *localServiceDiscovery) NetworkGateways() []model.NetworkGateway {
	// TODO implement fromRegistry logic from kube controller if needed
	return nil
}

func (l *localServiceDiscovery) MCSServices() []model.MCSServiceInfo {
	return nil
}

func TestPopulateFailoverPriorityLabels(t *testing.T) {
	tests := []struct {
		name           string
		dr             *config.Config
		mesh           *meshconfig.MeshConfig
		expectedLabels []byte
	}{
		{
			name:           "no dr",
			expectedLabels: nil,
		},
		{
			name: "simple",
			dr: &config.Config{
				Spec: &networking.DestinationRule{
					TrafficPolicy: &networking.TrafficPolicy{
						OutlierDetection: &networking.OutlierDetection{
							ConsecutiveErrors: 5,
						},
						LoadBalancer: &networking.LoadBalancerSettings{
							LocalityLbSetting: &networking.LocalityLoadBalancerSetting{
								FailoverPriority: []string{
									"a",
									"b",
								},
							},
						},
					},
				},
			},
			expectedLabels: []byte("a:a b:b "),
		},
		{
			name: "no outlier detection",
			dr: &config.Config{
				Spec: &networking.DestinationRule{
					TrafficPolicy: &networking.TrafficPolicy{
						LoadBalancer: &networking.LoadBalancerSettings{
							LocalityLbSetting: &networking.LocalityLoadBalancerSetting{
								FailoverPriority: []string{
									"a",
									"b",
								},
							},
						},
					},
				},
			},
			expectedLabels: nil,
		},
		{
			name: "no failover priority",
			dr: &config.Config{
				Spec: &networking.DestinationRule{
					TrafficPolicy: &networking.TrafficPolicy{
						OutlierDetection: &networking.OutlierDetection{
							ConsecutiveErrors: 5,
						},
					},
				},
			},
			expectedLabels: nil,
		},
		{
			name: "failover priority disabled",
			dr: &config.Config{
				Spec: &networking.DestinationRule{
					TrafficPolicy: &networking.TrafficPolicy{
						OutlierDetection: &networking.OutlierDetection{
							ConsecutiveErrors: 5,
						},
						LoadBalancer: &networking.LoadBalancerSettings{
							LocalityLbSetting: &networking.LocalityLoadBalancerSetting{
								FailoverPriority: []string{
									"a",
									"b",
								},
								Enabled: &wrapperspb.BoolValue{Value: false},
							},
						},
					},
				},
			},
			expectedLabels: nil,
		},
		{
			name: "mesh LocalityLoadBalancerSetting",
			dr: &config.Config{
				Spec: &networking.DestinationRule{
					TrafficPolicy: &networking.TrafficPolicy{
						OutlierDetection: &networking.OutlierDetection{
							ConsecutiveErrors: 5,
						},
					},
				},
			},
			mesh: &meshconfig.MeshConfig{
				LocalityLbSetting: &networking.LocalityLoadBalancerSetting{
					FailoverPriority: []string{
						"a",
						"b",
					},
				},
			},
			expectedLabels: []byte("a:a b:b "),
		},
		{
			name: "mesh LocalityLoadBalancerSetting(no outlier detection)",
			dr: &config.Config{
				Spec: &networking.DestinationRule{
					TrafficPolicy: &networking.TrafficPolicy{},
				},
			},
			mesh: &meshconfig.MeshConfig{
				LocalityLbSetting: &networking.LocalityLoadBalancerSetting{
					FailoverPriority: []string{
						"a",
						"b",
					},
				},
			},
			expectedLabels: nil,
		},
		{
			name: "mesh LocalityLoadBalancerSetting(no failover priority)",
			dr: &config.Config{
				Spec: &networking.DestinationRule{
					TrafficPolicy: &networking.TrafficPolicy{
						OutlierDetection: &networking.OutlierDetection{
							ConsecutiveErrors: 5,
						},
					},
				},
			},
			mesh: &meshconfig.MeshConfig{
				LocalityLbSetting: &networking.LocalityLoadBalancerSetting{},
			},
			expectedLabels: nil,
		},
		{
			name: "mesh LocalityLoadBalancerSetting(failover priority disabled)",
			dr: &config.Config{
				Spec: &networking.DestinationRule{
					TrafficPolicy: &networking.TrafficPolicy{
						OutlierDetection: &networking.OutlierDetection{
							ConsecutiveErrors: 5,
						},
					},
				},
			},
			mesh: &meshconfig.MeshConfig{
				LocalityLbSetting: &networking.LocalityLoadBalancerSetting{
					FailoverPriority: []string{
						"a",
						"b",
					},
					Enabled: &wrapperspb.BoolValue{Value: false},
				},
			},
			expectedLabels: nil,
		},
		{
			name: "both dr and mesh LocalityLoadBalancerSetting",
			dr: &config.Config{
				Spec: &networking.DestinationRule{
					TrafficPolicy: &networking.TrafficPolicy{
						OutlierDetection: &networking.OutlierDetection{
							ConsecutiveErrors: 5,
						},
						LoadBalancer: &networking.LoadBalancerSettings{
							LocalityLbSetting: &networking.LocalityLoadBalancerSetting{
								FailoverPriority: []string{
									"a",
									"b",
								},
							},
						},
					},
				},
			},
			mesh: &meshconfig.MeshConfig{
				LocalityLbSetting: &networking.LocalityLoadBalancerSetting{
					FailoverPriority: []string{
						"c",
					},
				},
			},
			expectedLabels: []byte("a:a b:b "),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := EndpointBuilder{
				proxy: &model.Proxy{
					Metadata: &model.NodeMetadata{},
					Labels: map[string]string{
						"app": "foo",
						"a":   "a",
						"b":   "b",
					},
				},
				push: &model.PushContext{
					Mesh: tt.mesh,
				},
			}
			if tt.dr != nil {
				b.destinationRule = model.ConvertConsolidatedDestRule(tt.dr, nil)
			}
			b.populateFailoverPriorityLabels()
			if !reflect.DeepEqual(b.failoverPriorityLabels, tt.expectedLabels) {
				t.Fatalf("expected priorityLabels %v but got %v", tt.expectedLabels, b.failoverPriorityLabels)
			}
		})
	}
}

func makeService(namespace, hostname string, scope model.ServiceScope) *model.ServiceInfo {
	svc := &workloadapi.Service{
		Namespace: namespace,
		Hostname:  hostname,
	}
	return &model.ServiceInfo{
		Service: svc,
		Scope:   scope,
	}
}

func TestFilterIstioEndpoint(t *testing.T) {
	svc := &model.Service{
		Hostname: "example.ns.svc.cluster.local",
		Attributes: model.ServiceAttributes{
			Name:      "example",
			Namespace: "ns",
			K8sAttributes: model.K8sAttributes{
				NodeLocal: false,
			},
		},
		Resolution: model.DNSLB,
		Ports:      model.PortList{{Port: 80, Protocol: protocol.HTTP, Name: "http"}},
	}
	localSvc := makeService("ns", "example.ns.svc.cluster.local", model.Local)
	globalSvc := makeService("ns", "example.ns.svc.cluster.local", model.Global)
	sidecar := &model.Proxy{
		Type:        model.SidecarProxy,
		IPAddresses: []string{"111.111.111.111", "1111:2222::1"},
		ID:          "v0.default",
		DNSDomain:   "example.org",
		Metadata: &model.NodeMetadata{
			Namespace: "not-default",
			NodeName:  "example",
		},
		ConfigNamespace: "not-default",
	}
	waypoint := &model.Proxy{
		Type: model.Waypoint,
		Metadata: &model.NodeMetadata{
			Namespace: "default",
			NodeName:  "example",
			ClusterID: "local",
		},
		Labels: map[string]string{label.GatewayManaged.Name: constants.ManagedGatewayMeshControllerLabel},
	}
	ingressGw := &model.Proxy{
		Type: model.Router,
		Metadata: &model.NodeMetadata{
			Namespace: "default",
			NodeName:  "example",
			ClusterID: "local",
		},
		Labels: map[string]string{label.GatewayManaged.Name: constants.ManagedGatewayControllerLabel},
	}
	ep0 := &model.IstioEndpoint{
		Addresses: []string{"1.1.1.1"},
		NodeName:  "example",
	}
	ep1 := &model.IstioEndpoint{
		Addresses: []string{"1.1.1.1"},
		NodeName:  "example",
	}
	ep2 := &model.IstioEndpoint{
		Addresses: []string{"2001:1::1"},
		NodeName:  "example",
	}
	ep3 := &model.IstioEndpoint{
		Addresses: []string{"1.1.1.1", "2001:1::1"},
		NodeName:  "example",
	}
	ep4 := &model.IstioEndpoint{
		Addresses: []string{},
		NodeName:  "example",
	}
	localEp := &model.IstioEndpoint{
		Addresses: []string{"1.1.1.1"},
		Locality:  model.Locality{ClusterID: "local"},
	}
	remoteEp := &model.IstioEndpoint{
		Addresses: []string{"1.1.1.1"},
		Locality:  model.Locality{ClusterID: "remote"},
	}

	tests := []struct {
		name     string
		proxy    *model.Proxy
		ep       *model.IstioEndpoint
		svcInfo  *model.ServiceInfo
		expected bool
	}{
		{
			name:     "test endpoint with different service port name",
			proxy:    sidecar,
			ep:       ep0,
			expected: true,
		},
		{
			name:     "test endpoint with ipv4 address",
			proxy:    sidecar,
			ep:       ep1,
			expected: true,
		},
		{
			name:     "test endpoint with ipv6 address",
			proxy:    sidecar,
			ep:       ep2,
			expected: true,
		},
		{
			name:     "test endpoint with both ipv4 and ipv6 addresses",
			proxy:    sidecar,
			ep:       ep3,
			expected: true,
		},
		{
			name:     "test endpoint without address",
			proxy:    sidecar,
			ep:       ep4,
			expected: false,
		},
		{
			name:     "test ambient endpoint in local cluster for global service",
			proxy:    waypoint,
			ep:       localEp,
			svcInfo:  globalSvc,
			expected: true,
		},
		{
			name:     "test ambient endpoint in remote cluster for global service",
			proxy:    waypoint,
			ep:       remoteEp,
			svcInfo:  globalSvc,
			expected: true,
		},
		{
			name:     "test ambient endpoint in local cluster for local service",
			proxy:    waypoint,
			ep:       localEp,
			svcInfo:  localSvc,
			expected: true,
		},
		{
			name:     "test ambient endpoint in remote cluster for local service",
			proxy:    waypoint,
			ep:       remoteEp,
			svcInfo:  localSvc,
			expected: false,
		},
		{
			name:     "test ambient endpoint in local cluster for global service",
			proxy:    ingressGw,
			ep:       localEp,
			svcInfo:  globalSvc,
			expected: true,
		},
		{
			name:     "test ambient endpoint in remote cluster for global service",
			proxy:    ingressGw,
			ep:       remoteEp,
			svcInfo:  globalSvc,
			expected: true,
		},
		{
			name:     "test ambient endpoint in local cluster for local service",
			proxy:    ingressGw,
			ep:       localEp,
			svcInfo:  localSvc,
			expected: true,
		},
		{
			name:     "test ambient endpoint in remote cluster for local service",
			proxy:    ingressGw,
			ep:       remoteEp,
			svcInfo:  localSvc,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := model.NewEnvironment()
			configStore := model.NewFakeStore()
			env.ConfigStore = configStore
			env.Watcher = meshwatcher.NewTestWatcher(&meshconfig.MeshConfig{RootNamespace: "istio-system"})
			meshNetworks := meshwatcher.NewFixedNetworksWatcher(nil)
			env.NetworksWatcher = meshNetworks
			env.ServiceDiscovery = memory.NewServiceDiscovery()
			xdsUpdater := xdsfake.NewFakeXDS()
			if err := env.InitNetworksManager(xdsUpdater); err != nil {
				t.Fatal(err)
			}
			if tt.svcInfo != nil {
				env.AmbientIndexes = &mockAmbientIndex{
					serviceInfos: []*model.ServiceInfo{tt.svcInfo},
				}
				test.SetForTest(t, &features.EnableAmbientMultiNetwork, true)
			}
			env.ServiceDiscovery = &localServiceDiscovery{
				services: []*model.Service{svc},
				serviceInstances: []*model.ServiceInstance{{
					Endpoint: tt.ep,
				}},
			}
			env.VirtualServiceController = model.NewVirtualServiceController(
				configStore,
				model.VSControllerOptions{KrtDebugger: krt.GlobalDebugHandler},
				env.Watcher,
			)
			stop := test.NewStop(t)
			go configStore.Run(stop)
			go env.VirtualServiceController.Run(stop)
			kube.WaitForCacheSync("test", stop, configStore.HasSynced)
			kube.WaitForCacheSync("test", stop, env.VirtualServiceController.HasSynced)
			env.Init()

			// Init a new push context
			push := model.NewPushContext()
			push.InitContext(env, nil, nil)
			env.SetPushContext(push)
			if push.NetworkManager() == nil {
				t.Fatal("error: NetworkManager should not be nil!")
			}

			tt.proxy.SetSidecarScope(push)

			builder := NewCDSEndpointBuilder(
				tt.proxy, push,
				"outbound||example.ns.svc.cluster.local",
				model.TrafficDirectionOutbound, "", "example.ns.svc.cluster.local", 80,
				svc, nil)
			expected := builder.filterIstioEndpoint(tt.ep)
			if !reflect.DeepEqual(tt.expected, expected) {
				t.Fatalf("expected  %v but got %v", tt.expected, expected)
			}
		})
	}
}

func TestBuildClusterLoadAssignment_InferenceServicePortFiltering(t *testing.T) {
	tests := []struct {
		name                 string
		InferencePoolService bool
		expectedEndpoints    int
	}{
		{
			name:                 "inference service includes endpoints from all ports",
			InferencePoolService: true,
			expectedEndpoints:    3,
		},
		{
			name:                 "regular service filters endpoints by port name",
			InferencePoolService: false,
			expectedEndpoints:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svcLabels := make(map[string]string)
			if tt.InferencePoolService {
				svcLabels[constants.InternalServiceSemantics] = constants.ServiceSemanticsInferencePool
			}

			svc := &model.Service{
				Hostname: "example.ns.svc.cluster.local",
				Attributes: model.ServiceAttributes{
					Name:      "example",
					Namespace: "ns",
					Labels:    svcLabels,
				},
				Ports: model.PortList{
					{Port: 80, Protocol: protocol.HTTP, Name: "http-80"},
					{Port: 8000, Protocol: protocol.HTTP, Name: "http-8000"},
					{Port: 8001, Protocol: protocol.HTTP, Name: "http-8001"},
				},
			}

			proxy := &model.Proxy{
				Type:        model.SidecarProxy,
				IPAddresses: []string{"127.0.0.1"},
				Metadata: &model.NodeMetadata{
					Namespace: "ns",
					NodeName:  "example",
				},
				ConfigNamespace: "ns",
			}

			endpointIndex := model.NewEndpointIndex(model.NewXdsCache())
			shards, _ := endpointIndex.GetOrCreateEndpointShard("example.ns.svc.cluster.local", "ns")
			shards.Lock()
			shards.Shards[model.ShardKey{Cluster: "cluster1"}] = []*model.IstioEndpoint{
				{
					Addresses:       []string{"10.0.0.1"},
					ServicePortName: "http-80",
					EndpointPort:    80,
					HostName:        "example.ns.svc.cluster.local",
					Namespace:       "ns",
				},
				{
					Addresses:       []string{"10.0.0.2"},
					ServicePortName: "http-8000",
					EndpointPort:    8000,
					HostName:        "example.ns.svc.cluster.local",
					Namespace:       "ns",
				},
				{
					Addresses:       []string{"10.0.0.3"},
					ServicePortName: "http-8001",
					EndpointPort:    8001,
					HostName:        "example.ns.svc.cluster.local",
					Namespace:       "ns",
				},
			}
			shards.Unlock()

			env := model.NewEnvironment()
			configStore := model.NewFakeStore()
			env.ConfigStore = configStore
			env.Watcher = meshwatcher.NewTestWatcher(&meshconfig.MeshConfig{RootNamespace: "istio-system"})
			meshNetworks := meshwatcher.NewFixedNetworksWatcher(nil)
			env.NetworksWatcher = meshNetworks
			env.ServiceDiscovery = &localServiceDiscovery{
				services: []*model.Service{svc},
			}
			xdsUpdater := xdsfake.NewFakeXDS()
			if err := env.InitNetworksManager(xdsUpdater); err != nil {
				t.Fatal(err)
			}
			env.VirtualServiceController = model.NewVirtualServiceController(
				configStore,
				model.VSControllerOptions{KrtDebugger: krt.GlobalDebugHandler},
				env.Watcher,
			)
			stop := test.NewStop(t)
			go configStore.Run(stop)
			go env.VirtualServiceController.Run(stop)
			kube.WaitForCacheSync("test", stop, configStore.HasSynced)
			kube.WaitForCacheSync("test", stop, env.VirtualServiceController.HasSynced)
			env.Init()

			push := model.NewPushContext()
			push.InitContext(env, nil, nil)
			env.SetPushContext(push)

			proxy.SetSidecarScope(push)

			builder := NewCDSEndpointBuilder(
				proxy, push,
				"outbound|80||example.ns.svc.cluster.local",
				model.TrafficDirectionOutbound, "", "example.ns.svc.cluster.local", 80,
				svc, nil)

			cla := builder.BuildClusterLoadAssignment(endpointIndex)

			var totalEndpoints int
			for _, localityLbEndpoints := range cla.Endpoints {
				totalEndpoints += len(localityLbEndpoints.LbEndpoints)
			}

			if totalEndpoints != tt.expectedEndpoints {
				t.Errorf("expected %d endpoints, got %d", tt.expectedEndpoints, totalEndpoints)
			}
		})
	}
}

// TestServicesForLocalCluster verifies that the local cluster service set is deduplicated across
// service-ports by hostname and sorted by hostname.
func TestServicesForLocalCluster(t *testing.T) {
	mkSvc := func(hostname string) *model.Service {
		return &model.Service{Hostname: host.Name(hostname)}
	}
	svcB := mkSvc("b.example.com")
	svcA := mkSvc("a.example.com")

	// Two ports on svcB -> one entry per port, both pointing at the same Service.
	port80 := &model.Port{Name: "http", Port: 80, Protocol: protocol.HTTP}
	port443 := &model.Port{Name: "https", Port: 443, Protocol: protocol.HTTP}
	instPort := func(p *model.Port) model.ServiceInstancePort {
		return model.ServiceInstancePort{ServicePort: p, TargetPort: uint32(p.Port)}
	}

	proxy := &model.Proxy{
		ServiceTargets: []model.ServiceTarget{
			{Service: svcB, Port: instPort(port80)},
			{Service: svcB, Port: instPort(port443)}, // duplicate of svcB, different port
			{Service: svcA, Port: instPort(port80)},
			{Service: nil, Port: instPort(port80)}, // ignored
		},
	}

	got := ServicesForLocalCluster(proxy)
	// Sorted by hostname: a.example.com, b.example.com
	want := []*model.Service{svcA, svcB}
	if len(got) != len(want) {
		t.Fatalf("got %d services %v, want %d", len(got), hostnamesOf(got), len(want))
	}
	for i, svc := range want {
		if got[i].Hostname != svc.Hostname {
			t.Errorf("[%d] got %s, want %s", i, got[i].Hostname, svc.Hostname)
		}
	}
}

func hostnamesOf(svcs []*model.Service) []string {
	out := make([]string, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, string(s.Hostname)+"/"+s.Attributes.Namespace)
	}
	return out
}

// TestGroupingKeyForLocalCluster verifies the grouping key construction.
func TestGroupingKeyForLocalCluster(t *testing.T) {
	tests := []struct {
		name    string
		names   []string
		labels  map[string]string
		wantKey string
	}{
		{
			name:    "default pod-template-hash",
			names:   []string{"pod-template-hash"},
			labels:  map[string]string{"pod-template-hash": "abc"},
			wantKey: "pod-template-hash:abc",
		},
		{
			name:    "missing label yields empty value",
			names:   []string{"pod-template-hash"},
			labels:  map[string]string{},
			wantKey: "pod-template-hash:",
		},
		{
			name:    "multiple labels joined in order",
			names:   []string{"app.kubernetes.io/instance", "app.kubernetes.io/version"},
			labels:  map[string]string{"app.kubernetes.io/instance": "x", "app.kubernetes.io/version": "1"},
			wantKey: "app.kubernetes.io/instance:x,app.kubernetes.io/version:1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := groupingKeyForLocalCluster(tt.names, tt.labels); got != tt.wantKey {
				t.Errorf("got %q, want %q", got, tt.wantKey)
			}
		})
	}
}

// TestFilterIstioEndpointForLocalCluster verifies grouping-key matching and address dedupe
func TestFilterIstioEndpointForLocalCluster(t *testing.T) {
	proxyByHash := &model.Proxy{
		Type:      model.SidecarProxy,
		DNSDomain: "ns.svc.cluster.local",
		Metadata: &model.NodeMetadata{
			Namespace:           "ns",
			NodeName:            "node",
			WorkloadName:        "example",
			EnableSelfDiscovery: true,
		},
		Labels: map[string]string{"pod-template-hash": "abcdefgh"},
	}

	proxyByInstanceByVersion := &model.Proxy{
		Type:      model.SidecarProxy,
		DNSDomain: "ns.svc.cluster.local",
		Metadata: &model.NodeMetadata{
			Namespace:                       "ns",
			NodeName:                        "node",
			WorkloadName:                    "example",
			SelfDiscoveryGroupingLabelNames: "app.kubernetes.io/instance,app.kubernetes.io/version",
		},
		Labels: map[string]string{
			"app.kubernetes.io/instance": "example-primary",
			"app.kubernetes.io/version":  "0.1",
			"pod-template-hash":          "abcdefgh",
		},
	}

	tests := []struct {
		name     string
		proxy    *model.Proxy
		ep       *model.IstioEndpoint
		addrsMap map[string]struct{}
		expected bool
	}{
		{
			name:  "by hash, same grouping key, empty map",
			proxy: proxyByHash,
			ep: &model.IstioEndpoint{
				Addresses: []string{"1.1.1.1", "2001:1::1"},
				Labels:    map[string]string{"pod-template-hash": "abcdefgh"},
			},
			addrsMap: map[string]struct{}{},
			expected: true,
		},
		{
			name:  "by hash, same grouping key, duplicate address",
			proxy: proxyByHash,
			ep: &model.IstioEndpoint{
				Addresses: []string{"1.1.1.1", "2001:1::1"},
				Labels:    map[string]string{"pod-template-hash": "abcdefgh"},
			},
			addrsMap: map[string]struct{}{"1.1.1.1": {}},
			expected: false,
		},
		{
			name:  "by hash, different grouping key (canary ReplicaSet), excluded",
			proxy: proxyByHash,
			ep: &model.IstioEndpoint{
				Addresses: []string{"1.1.1.1", "2001:1::1"},
				Labels:    map[string]string{"pod-template-hash": "zyxwvuts"},
			},
			addrsMap: map[string]struct{}{},
			expected: false,
		},
		{
			name:  "by instance by version, same grouping key",
			proxy: proxyByInstanceByVersion,
			ep: &model.IstioEndpoint{
				Addresses: []string{"1.1.1.1", "2001:1::1"},
				Labels: map[string]string{
					"app.kubernetes.io/instance": "example-primary",
					"app.kubernetes.io/version":  "0.1",
				},
			},
			addrsMap: map[string]struct{}{},
			expected: true,
		},
		{
			name:  "by instance by version, different instance (canary), excluded",
			proxy: proxyByInstanceByVersion,
			ep: &model.IstioEndpoint{
				Addresses: []string{"1.1.1.1", "2001:1::1"},
				Labels: map[string]string{
					"app.kubernetes.io/instance": "example-canary",
					"app.kubernetes.io/version":  "0.1",
				},
			},
			addrsMap: map[string]struct{}{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// filterIstioEndpointForLocalCluster only depends on the grouping key (subsetName) and the
			// configured label list, so exercise it via a minimal builder without the heavy push/sidecar setup.
			labels := groupingLabelNamesForLocalCluster(tt.proxy)
			builder := &EndpointBuilder{
				subsetName:                     groupingKeyForLocalCluster(labels, tt.proxy.Labels),
				localClusterGroupingLabelNames: labels,
			}
			got := builder.filterIstioEndpointForLocalCluster(tt.ep, tt.addrsMap)
			if got != tt.expected {
				t.Fatalf("expected %v but got %v", tt.expected, got)
			}
		})
	}
}

// TestCopyIstioEndpointForLocalCluster verifies the dummy-port rewrite and that the original
// endpoint is not mutated.
func TestCopyIstioEndpointForLocalCluster(t *testing.T) {
	ep := &model.IstioEndpoint{
		Addresses:       []string{"1.1.1.1"},
		ServicePortName: "http",
		EndpointPort:    80,
	}
	got := copyIstioEndpointForLocalCluster(ep)
	if got.ServicePortName != localClusterPortName {
		t.Errorf("ServicePortName = %q, want %q", got.ServicePortName, localClusterPortName)
	}
	if got.EndpointPort != LocalClusterPortNumber {
		t.Errorf("EndpointPort = %d, want %d", got.EndpointPort, LocalClusterPortNumber)
	}
	// original must be untouched
	if ep.ServicePortName != "http" || ep.EndpointPort != 80 {
		t.Errorf("original endpoint was mutated: %v", ep)
	}
}
