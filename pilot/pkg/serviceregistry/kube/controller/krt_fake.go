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
	corev1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/aggregate"
	"istio.io/istio/pilot/pkg/serviceregistry/util/xdsfake"
	"istio.io/istio/pkg/activenotifier"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/config/schema/gvr"
	kubelib "istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/kclient/clienttest"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/kubetypes"
	"istio.io/istio/pkg/kube/namespace"
	"istio.io/istio/pkg/test"
)

// FakeKrtControllerOptions mirrors FakeControllerOptions but is used for the KRT-based controller.
type FakeKrtControllerOptions struct {
	Client            kubelib.Client
	CRDs              []schema.GroupVersionResource
	NetworksWatcher   meshwatcher.NetworksWatcherCollection
	MeshWatcher       meshwatcher.WatcherCollection
	ServiceHandler    model.ServiceHandler
	ClusterID         cluster.ID
	WatchedNamespaces string
	DomainSuffix      string
	XDSUpdater        model.XDSUpdater
	Stop              chan struct{}
	SkipRun           bool
	ConfigCluster     bool
	SystemNamespace   string
}

// fakeClients holds the kclients used to manipulate the underlying state in tests.
type fakeClients struct {
	pods           kclient.Client[*corev1.Pod]
	services       kclient.Client[*corev1.Service]
	nodes          kclient.Client[*corev1.Node]
	namespaces     kclient.Client[*corev1.Namespace]
	endpointSlices kclient.Client[*discovery.EndpointSlice]
	// Gateways are watched via the dynamic informer so that the CRD does not need to be
	// installed when not using gateway-api.
	gateways kclient.Informer[*gatewayv1.Gateway]
}

// fakeClusterCollections wraps krt collections derived from the test kclients and
// implements ClusterCollections.
type fakeClusterCollections struct {
	namespaces     krt.Collection[*corev1.Namespace]
	pods           krt.Collection[*corev1.Pod]
	services       krt.Collection[*corev1.Service]
	endpointSlices krt.Collection[*discovery.EndpointSlice]
	nodes          krt.Collection[*corev1.Node]
	gateways       krt.Collection[*gatewayv1.Gateway]
}

func (c *fakeClusterCollections) Namespaces() krt.Collection[*corev1.Namespace] { return c.namespaces }
func (c *fakeClusterCollections) Pods() krt.Collection[*corev1.Pod]             { return c.pods }
func (c *fakeClusterCollections) Services() krt.Collection[*corev1.Service]     { return c.services }
func (c *fakeClusterCollections) EndpointSlices() krt.Collection[*discovery.EndpointSlice] {
	return c.endpointSlices
}
func (c *fakeClusterCollections) Nodes() krt.Collection[*corev1.Node]          { return c.nodes }
func (c *fakeClusterCollections) Gateways() krt.Collection[*gatewayv1.Gateway] { return c.gateways }

// FakeKrtController bundles a KrtController with the kclients used to manipulate
// kube state and the krt collections it consumes.
type FakeKrtController struct {
	*KrtController
	Endpoints   *model.EndpointIndex
	Clients     *fakeClients
	Collections *fakeClusterCollections
}

// NewFakeKrtControllerWithOptions builds a KrtController wired to fake kube clients,
// kicked off through fake KRT collections.
func NewFakeKrtControllerWithOptions(t test.Failer, opts FakeKrtControllerOptions) (*FakeKrtController, *xdsfake.Updater) {
	xdsUpdater := opts.XDSUpdater
	var endpoints *model.EndpointIndex
	if xdsUpdater == nil {
		endpoints = model.NewEndpointIndex(model.DisabledCache{})
		delegate := model.NewEndpointIndexUpdater(endpoints)
		xdsUpdater = xdsfake.NewWithDelegate(delegate)
	}

	domainSuffix := defaultFakeDomainSuffix
	if opts.DomainSuffix != "" {
		domainSuffix = opts.DomainSuffix
	}
	if opts.Client == nil {
		opts.Client = kubelib.NewFakeClient()
	}
	if opts.ClusterID == "" {
		opts.ClusterID = opts.Client.ClusterID()
	}
	if opts.MeshWatcher == nil {
		opts.MeshWatcher = meshwatcher.NewTestWatcher(&meshconfig.MeshConfig{TrustDomain: "cluster.local"})
	}
	if opts.NetworksWatcher == nil {
		opts.NetworksWatcher = meshwatcher.NewFixedNetworksWatcher(nil)
	}

	cleanupStop := false
	stop := opts.Stop
	if stop == nil {
		cleanupStop = true
		stop = make(chan struct{})
	}

	// Set up discovery namespaces filter, identical to fake.go.
	f := namespace.NewDiscoveryNamespacesFilter(
		kclient.New[*corev1.Namespace](opts.Client),
		opts.MeshWatcher,
		stop,
	)
	kubelib.SetObjectFilter(opts.Client, f)

	for _, crd := range opts.CRDs {
		clienttest.MakeCRD(t, opts.Client, crd)
	}

	var configCluster cluster.ID
	if opts.ConfigCluster {
		configCluster = opts.ClusterID
	}
	meshServiceController := aggregate.NewController(aggregate.Options{
		MeshHolder:      opts.MeshWatcher,
		ConfigClusterID: configCluster,
	})

	kopts := krt.NewOptionsBuilder(stop, "FakeKubeServiceRegistry", krt.GlobalDebugHandler)

	defaultFilter := kclient.Filter{ObjectFilter: opts.Client.ObjectFilter()}

	// Create kclients that tests use to manipulate state.
	clients := &fakeClients{
		pods: kclient.NewFiltered[*corev1.Pod](opts.Client, kclient.Filter{
			ObjectFilter:    opts.Client.ObjectFilter(),
			ObjectTransform: kubelib.StripPodUnusedFields,
			FieldSelector:   "status.phase!=Failed",
		}),
		services: kclient.NewFiltered[*corev1.Service](opts.Client, defaultFilter),
		nodes: kclient.NewFiltered[*corev1.Node](opts.Client, kclient.Filter{
			ObjectTransform: kubelib.StripNodeUnusedFields,
		}),
		namespaces:     kclient.New[*corev1.Namespace](opts.Client),
		endpointSlices: kclient.NewFiltered[*discovery.EndpointSlice](opts.Client, defaultFilter),
		gateways: kclient.NewDelayedInformer[*gatewayv1.Gateway](
			opts.Client, gvr.KubernetesGateway, kubetypes.StandardInformer, defaultFilter,
		),
	}

	// Wrap them as KRT collections - each receives its own name to assist debugging.
	collections := &fakeClusterCollections{
		namespaces:     krt.WrapClient(clients.namespaces, kopts.WithName("informer/Namespaces")...),
		pods:           krt.WrapClient(clients.pods, kopts.WithName("informer/Pods")...),
		services:       krt.WrapClient(clients.services, kopts.WithName("informer/Services")...),
		endpointSlices: krt.WrapClient(clients.endpointSlices, kopts.WithName("informer/EndpointSlices")...),
		nodes:          krt.WrapClient(clients.nodes, kopts.WithName("informer/Nodes")...),
		gateways:       krt.WrapClient(clients.gateways, kopts.WithName("informer/Gateways")...),
	}

	options := Options{
		DomainSuffix:          domainSuffix,
		XDSUpdater:            xdsUpdater,
		Metrics:               &model.Environment{},
		MeshNetworksWatcher:   opts.NetworksWatcher,
		MeshWatcher:           opts.MeshWatcher,
		ClusterID:             opts.ClusterID,
		MeshServiceController: meshServiceController,
		ConfigCluster:         opts.ConfigCluster,
		SystemNamespace:       opts.SystemNamespace,
		StatusWritingEnabled:  activenotifier.New(false),
		KrtDebugger:           krt.GlobalDebugHandler,
	}

	features := Features{
		EnableK8SServiceSelectWorkloadEntries: features.EnableK8SServiceSelectWorkloadEntries,
		EnableProxyFindPodByIP:                features.EnableProxyFindPodByIP,
		EnableDualStack:                       features.EnableDualStack,
		GlobalSendUnhealthyEndpoints:          features.GlobalSendUnhealthyEndpoints.Load(),
		EnableMCSServiceDiscovery:             features.EnableMCSServiceDiscovery,
		EnableMCSClusterLocal:                 features.EnableMCSClusterLocal,
		MultiNetworkGatewayAPI:                features.MultiNetworkGatewayAPI,
		EnableMCSHost:                         features.EnableMCSHost,
	}

	c := NewKrtController(opts.Client, collections, features, options)
	meshServiceController.AddRegistry(c)

	if opts.ServiceHandler != nil {
		c.AppendServiceHandler(opts.ServiceHandler)
	}

	t.Cleanup(func() {
		opts.Client.Shutdown()
	})

	if cleanupStop {
		t.Cleanup(func() {
			close(stop)
		})
	}

	opts.Client.RunAndWait(stop)
	var fx *xdsfake.Updater
	if x, ok := xdsUpdater.(*xdsfake.Updater); ok {
		fx = x
	}

	if !opts.SkipRun {
		go c.Run(stop)
		kubelib.WaitForCacheSync("test", stop, c.HasSynced)
	}

	return &FakeKrtController{
		KrtController: c,
		Endpoints:     endpoints,
		Clients:       clients,
		Collections:   collections,
	}, fx
}
