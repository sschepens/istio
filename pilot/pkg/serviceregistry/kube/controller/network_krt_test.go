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

package controller

import (
	"strconv"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	k8sv1 "sigs.k8s.io/gateway-api/apis/v1"

	"istio.io/api/label"
	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/util/xdsfake"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvr"
	kubelib "istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/kclient/clienttest"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/kubetypes"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
)

func newTestKrtNetworkManager(t *testing.T, meshNetworks meshwatcher.TestNetworksWatcher) (
	*krtNetworkManager,
	krt.StaticCollection[*model.Service],
	kubelib.Client,
) {
	test.SetForTest(t, &features.MultiNetworkGatewayAPI, true)
	stop := test.NewStop(t)

	client := kubelib.NewFakeClient()
	clienttest.MakeCRD(t, client, gvr.KubernetesGateway)

	services := krt.NewStaticCollection[*model.Service](nil, nil,
		krt.WithStop(stop), krt.WithDebugging(krt.GlobalDebugHandler))

	nsClient := kclient.New[*corev1.Namespace](client)
	namespaces := krt.WrapClient(nsClient, krt.WithStop(stop), krt.WithDebugging(krt.GlobalDebugHandler))

	opts := krt.NewOptionsBuilder(stop, "", krt.GlobalDebugHandler)

	gatewayClient := kclient.NewDelayedInformer[*gatewayv1.Gateway](client, gvr.KubernetesGateway, kubetypes.StandardInformer, kclient.Filter{
		ObjectFilter: client.ObjectFilter(),
	})
	gateways := krt.WrapClient(gatewayClient, opts.WithName("informer/Gateways")...)

	client.RunAndWait(stop)

	n := newKrtNetworkManager(
		services,
		namespaces,
		gateways,
		meshNetworks,
		xdsfake.NewFakeXDS(),
		"istio-system",
		constants.DefaultClusterName,
		false,
		Features{
			MultiNetworkGatewayAPI: features.MultiNetworkGatewayAPI,
		},
		opts,
	)

	kubelib.WaitForCacheSync("test", stop, n.HasSynced)

	return n, services, client
}

func TestKrtNetworkUpdateTriggers(t *testing.T) {
	meshNetworks := meshwatcher.NewFixedNetworksWatcher(nil)
	n, services, client := newTestKrtNetworkManager(t, meshNetworks)

	if len(n.NetworkGateways()) != 0 {
		t.Fatal("did not expect any gateways yet")
	}

	notifyCh := make(chan struct{}, 10)
	var (
		gwMu sync.Mutex
		gws  []model.NetworkGateway
	)
	setGws := func(v []model.NetworkGateway) {
		gwMu.Lock()
		defer gwMu.Unlock()
		gws = v
	}
	getGws := func() []model.NetworkGateway {
		gwMu.Lock()
		defer gwMu.Unlock()
		return gws
	}

	n.AppendNetworkGatewayHandler(func() {
		setGws(n.NetworkGateways())
		notifyCh <- struct{}{}
	})
	expectGateways := func(t *testing.T, expectedGws int) {
		t.Helper()
		for range 3 {
			assert.ChannelHasItem(t, notifyCh)
			if n := len(getGws()); n == expectedGws {
				return
			}
		}
		t.Errorf("expected %d gateways but got %v", expectedGws, getGws())
	}

	t.Run("add meshnetworks", func(t *testing.T) {
		addKrtMeshNetworksFromRegistryGateway(t, services, meshNetworks)
		expectGateways(t, 3)
	})
	t.Run("add labeled service", func(t *testing.T) {
		addKrtLabeledServiceGateway(t, services, "nw0")
		expectGateways(t, 4)
	})
	t.Run("update labeled service network", func(t *testing.T) {
		addKrtLabeledServiceGateway(t, services, "nw1")
		expectGateways(t, 4)
	})
	t.Run("add kubernetes gateway", func(t *testing.T) {
		addOrUpdateKrtGatewayResource(t, client, 35443)
		expectGateways(t, 8)
	})
	t.Run("update kubernetes gateway", func(t *testing.T) {
		addOrUpdateKrtGatewayResource(t, client, 45443)
		expectGateways(t, 8)
	})
	t.Run("remove kubernetes gateway", func(t *testing.T) {
		removeKrtGatewayResource(t, client)
		expectGateways(t, 4)
	})
	t.Run("remove labeled service", func(t *testing.T) {
		removeKrtLabeledServiceGateway(t, services)
		expectGateways(t, 3)
	})
	// gateways are created even without service
	t.Run("add kubernetes gateway", func(t *testing.T) {
		addOrUpdateKrtGatewayResource(t, client, 35443)
		expectGateways(t, 7)
	})
	t.Run("remove kubernetes gateway", func(t *testing.T) {
		removeKrtGatewayResource(t, client)
		expectGateways(t, 3)
	})
	t.Run("remove meshnetworks", func(t *testing.T) {
		meshNetworks.SetNetworks(nil)
		expectGateways(t, 0)
	})
}

func addKrtLabeledServiceGateway(t *testing.T, services krt.StaticCollection[*model.Service], nw string) {
	t.Helper()
	svc := &model.Service{
		Hostname: "istio-labeled-gw.arbitrary-ns.svc.cluster.local",
		Ports: model.PortList{
			{Name: "tcp", Port: 15443, Protocol: protocol.TCP},
		},
		Attributes: model.ServiceAttributes{
			Name:      "istio-labeled-gw",
			Namespace: "arbitrary-ns",
			Labels: map[string]string{
				label.TopologyNetwork.Name: nw,
			},
			K8sAttributes: model.K8sAttributes{
				ObjectName: "istio-labeled-gw",
			},
		},
	}
	svc.Attributes.ClusterExternalAddresses.SetAddressesFor(constants.DefaultClusterName, []string{"2.3.4.6"})
	services.UpdateObject(svc)
}

func removeKrtLabeledServiceGateway(t *testing.T, services krt.StaticCollection[*model.Service]) {
	t.Helper()
	svc := &model.Service{
		Hostname: "istio-labeled-gw.arbitrary-ns.svc.cluster.local",
		Attributes: model.ServiceAttributes{
			Name:      "istio-labeled-gw",
			Namespace: "arbitrary-ns",
			K8sAttributes: model.K8sAttributes{
				ObjectName: "istio-labeled-gw",
			},
		},
	}
	services.DeleteObject(krt.GetKey(svc))
}

func addOrUpdateKrtGatewayResource(t *testing.T, client kubelib.Client, customPort int) {
	t.Helper()
	passthroughMode := k8sv1.TLSModePassthrough
	ipType := k8sv1.IPAddressType
	hostnameType := k8sv1.HostnameAddressType
	clienttest.Wrap(t, kclient.New[*k8sv1.Gateway](client)).CreateOrUpdate(&k8sv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "eastwest-gwapi",
			Namespace: "istio-system",
			Labels:    map[string]string{label.TopologyNetwork.Name: "nw2"},
		},
		Spec: k8sv1.GatewaySpec{
			GatewayClassName: "istio",
			Addresses: []k8sv1.GatewaySpecAddress{
				{Type: &ipType, Value: "1.2.3.4"},
				{Type: &hostnameType, Value: "some hostname"},
			},
			Listeners: []k8sv1.Listener{
				{
					Name: "detected-by-options",
					TLS: &k8sv1.ListenerTLSConfig{
						Mode: &passthroughMode,
						Options: map[k8sv1.AnnotationKey]k8sv1.AnnotationValue{
							constants.ListenerModeOption: constants.ListenerModeAutoPassthrough,
						},
					},
					Port: k8sv1.PortNumber(customPort),
				},
				{
					Name: "detected-by-number",
					TLS:  &k8sv1.ListenerTLSConfig{Mode: &passthroughMode},
					Port: 15443,
				},
			},
		},
	})
}

func removeKrtGatewayResource(t *testing.T, client kubelib.Client) {
	t.Helper()
	clienttest.Wrap(t, kclient.New[*k8sv1.Gateway](client)).Delete("eastwest-gwapi", "istio-system")
}

// TestKrtMeshNetworksCIDRMatching mirrors TestMeshNetworksCIDRMatching from the
// legacy network manager. It exercises the CIDR-based fallback tier of
// krtNetworkManager.Network():
//  1. an endpoint IP matching exactly one network → that network is returned
//  2. an endpoint IP matching no network → empty
//  3. an endpoint IP inside overlapping CIDRs → first match wins (warning logged)
//
// Networks intentionally omit fromRegistry so we isolate the CIDR tier; if
// fromRegistry matched this cluster, NetworkFromMeshConfig would short-circuit
// the CIDR lookup.
func TestKrtMeshNetworksCIDRMatching(t *testing.T) {
	meshNetworks := meshwatcher.NewFixedNetworksWatcher(&meshconfig.MeshNetworks{
		Networks: map[string]*meshconfig.Network{
			"net-a": {
				Endpoints: []*meshconfig.Network_NetworkEndpoints{
					{Ne: &meshconfig.Network_NetworkEndpoints_FromCidr{FromCidr: "10.10.0.0/16"}},
				},
			},
			"net-b": {
				Endpoints: []*meshconfig.Network_NetworkEndpoints{
					{Ne: &meshconfig.Network_NetworkEndpoints_FromCidr{FromCidr: "10.20.0.0/16"}},
				},
			},
			// Overlaps with net-a; cidranger returns both and the manager picks the first.
			"net-overlap": {
				Endpoints: []*meshconfig.Network_NetworkEndpoints{
					{Ne: &meshconfig.Network_NetworkEndpoints_FromCidr{FromCidr: "10.10.5.0/24"}},
				},
			},
		},
	})
	n, _, _ := newTestKrtNetworkManager(t, meshNetworks)

	// 1. Exact match.
	assert.Equal(t, n.Network("10.20.0.5", nil), "net-b")

	// 2. No match.
	assert.Equal(t, n.Network("192.168.10.10", nil), "")

	// 3. Overlapping CIDRs: result is implementation-defined first-match.
	got := n.Network("10.10.5.7", nil)
	if got != "net-a" && got != "net-overlap" {
		t.Fatalf("expected one of [net-a, net-overlap] for overlapping CIDRs, got %q", got)
	}
}

// TestKrtNetworkGatewaysFromServiceAndGatewayResource mirrors its legacy
// counterpart: gateways discovered through labeled Services and through
// Kubernetes Gateway API resources are merged into a single, sorted set, and
// duplicate Service contributions for the same (network, addr, port) collapse
// inside the gateway Set.
func TestKrtNetworkGatewaysFromServiceAndGatewayResource(t *testing.T) {
	meshNetworks := meshwatcher.NewFixedNetworksWatcher(nil)
	n, services, client := newTestKrtNetworkManager(t, meshNetworks)

	// Two services with identical (addr, port) → must dedup in the gateway set.
	addKrtLabeledServiceGatewayNamed(t, services, "gw-svc-a", "nw1", "10.0.0.1")
	addKrtLabeledServiceGatewayNamed(t, services, "gw-svc-b", "nw1", "10.0.0.1")

	// One Kubernetes Gateway resource on a distinct address, also for nw1.
	passthrough := k8sv1.TLSModePassthrough
	ipType := k8sv1.IPAddressType
	clienttest.Wrap(t, kclient.New[*k8sv1.Gateway](client)).CreateOrUpdate(&k8sv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gw-resource",
			Namespace: "istio-system",
			Labels:    map[string]string{label.TopologyNetwork.Name: "nw1"},
		},
		Spec: k8sv1.GatewaySpec{
			GatewayClassName: "istio",
			Addresses:        []k8sv1.GatewaySpecAddress{{Type: &ipType, Value: "10.0.0.99"}},
			Listeners: []k8sv1.Listener{{
				Name: "tls",
				TLS: &k8sv1.ListenerTLSConfig{
					Mode: &passthrough,
					Options: map[k8sv1.AnnotationKey]k8sv1.AnnotationValue{
						constants.ListenerModeOption: constants.ListenerModeAutoPassthrough,
					},
				},
				Port: 15443,
			}},
		},
	})

	assert.EventuallyEqual(t, func() []string {
		gws := n.NetworkGateways()
		out := make([]string, 0, len(gws))
		for _, gw := range gws {
			out = append(out, gwTriple(gw))
		}
		return out
	}, []string{"nw1|10.0.0.1|15443", "nw1|10.0.0.99|15443"})
}

// TestKrtInvalidMeshNetworksConfig mirrors its legacy counterpart: a malformed
// CIDR is skipped (logged & ignored) without panicking the manager, and other
// valid networks declared in the same config continue to resolve normally.
func TestKrtInvalidMeshNetworksConfig(t *testing.T) {
	meshNetworks := meshwatcher.NewFixedNetworksWatcher(&meshconfig.MeshNetworks{
		Networks: map[string]*meshconfig.Network{
			"valid-net": {
				Endpoints: []*meshconfig.Network_NetworkEndpoints{
					{Ne: &meshconfig.Network_NetworkEndpoints_FromCidr{FromCidr: "172.16.0.0/16"}},
				},
			},
			"bad-net": {
				Endpoints: []*meshconfig.Network_NetworkEndpoints{
					{Ne: &meshconfig.Network_NetworkEndpoints_FromCidr{FromCidr: "not a cidr"}},
				},
			},
		},
	})
	n, _, _ := newTestKrtNetworkManager(t, meshNetworks)

	assert.Equal(t, n.Network("172.16.5.5", nil), "valid-net")
	assert.Equal(t, n.Network("10.0.0.1", nil), "")
}

// addKrtLabeledServiceGatewayNamed is a parameterized variant of
// addKrtLabeledServiceGateway that allows naming the service and choosing its
// external address — needed when the test wants to register multiple gateway
// services with overlapping or distinct gateway data.
func addKrtLabeledServiceGatewayNamed(t *testing.T, services krt.StaticCollection[*model.Service], name, nw, addr string) {
	t.Helper()
	svc := &model.Service{
		Hostname: host.Name(name + ".istio-system.svc.cluster.local"),
		Ports: model.PortList{
			{Name: "tcp", Port: 15443, Protocol: protocol.TCP},
		},
		Attributes: model.ServiceAttributes{
			Name:      name,
			Namespace: "istio-system",
			Labels: map[string]string{
				label.TopologyNetwork.Name: nw,
			},
			K8sAttributes: model.K8sAttributes{
				ObjectName: name,
			},
		},
	}
	svc.Attributes.ClusterExternalAddresses.SetAddressesFor(constants.DefaultClusterName, []string{addr})
	services.UpdateObject(svc)
}

func gwTriple(gw model.NetworkGateway) string {
	return string(gw.Network) + "|" + gw.Addr + "|" + strconv.FormatUint(uint64(gw.Port), 10)
}

func addKrtMeshNetworksFromRegistryGateway(
	t *testing.T,
	services krt.StaticCollection[*model.Service],
	watcher meshwatcher.TestNetworksWatcher,
) {
	t.Helper()
	svc1 := &model.Service{
		Hostname: "istio-meshnetworks-gw.istio-system.svc.cluster.local",
		Ports: model.PortList{
			{Name: "tcp", Port: 15443, Protocol: protocol.TCP},
		},
		Attributes: model.ServiceAttributes{
			Name:      "istio-meshnetworks-gw",
			Namespace: "istio-system",
			K8sAttributes: model.K8sAttributes{
				ObjectName: "istio-meshnetworks-gw",
			},
		},
	}
	svc1.Attributes.ClusterExternalAddresses.SetAddressesFor(constants.DefaultClusterName, []string{"1.2.3.4"})

	svc2 := &model.Service{
		Hostname: "istio-meshnetworks-gw-2.istio-system.svc.cluster.local",
		Ports: model.PortList{
			{Name: "tcp", Port: 15443, Protocol: protocol.TCP},
		},
		Attributes: model.ServiceAttributes{
			Name:      "istio-meshnetworks-gw-2",
			Namespace: "istio-system",
			K8sAttributes: model.K8sAttributes{
				ObjectName: "istio-meshnetworks-gw-2",
			},
		},
	}
	svc2.Attributes.ClusterExternalAddresses.SetAddressesFor(constants.DefaultClusterName, []string{"1.2.3.5"})

	services.UpdateObject(svc1)
	services.UpdateObject(svc2)

	watcher.SetNetworks(&meshconfig.MeshNetworks{Networks: map[string]*meshconfig.Network{
		"nw0": {
			Endpoints: []*meshconfig.Network_NetworkEndpoints{{
				Ne: &meshconfig.Network_NetworkEndpoints_FromRegistry{FromRegistry: "Kubernetes"},
			}},
			Gateways: []*meshconfig.Network_IstioNetworkGateway{{
				Port: 15443,
				Gw:   &meshconfig.Network_IstioNetworkGateway_RegistryServiceName{RegistryServiceName: "istio-meshnetworks-gw.istio-system.svc.cluster.local"},
			}},
		},
		"nw1": {
			Endpoints: []*meshconfig.Network_NetworkEndpoints{{
				Ne: &meshconfig.Network_NetworkEndpoints_FromRegistry{FromRegistry: "Kubernetes"},
			}},
			Gateways: []*meshconfig.Network_IstioNetworkGateway{{
				Port: 15443,
				Gw:   &meshconfig.Network_IstioNetworkGateway_RegistryServiceName{RegistryServiceName: "istio-meshnetworks-gw.istio-system.svc.cluster.local"},
			}},
		},
		"nw2": {
			Endpoints: []*meshconfig.Network_NetworkEndpoints{{
				Ne: &meshconfig.Network_NetworkEndpoints_FromRegistry{FromRegistry: "Kubernetes"},
			}},
			Gateways: []*meshconfig.Network_IstioNetworkGateway{{
				Port: 15443,
				Gw:   &meshconfig.Network_IstioNetworkGateway_RegistryServiceName{RegistryServiceName: "istio-meshnetworks-gw-2.istio-system.svc.cluster.local"},
			}},
		},
	}})
}
