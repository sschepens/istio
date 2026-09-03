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

package serviceentry

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"

	"istio.io/api/label"
	meshconfig "istio.io/api/mesh/v1alpha1"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/config/memory"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/util/xdsfake"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/multicluster"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/retry"
)

const (
	configClusterID = cluster.ID("config-cluster")
	remoteClusterID = cluster.ID("remote-cluster")
	systemNamespace = "istio-system"
)

// namespaceWithNetwork is a system namespace carrying the topology label that names the network its
// cluster's workloads belong to.
func namespaceWithNetwork(name, nw string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{label.TopologyNetwork.Name: nw},
		},
	}
}

func nodeWithLocality(name, region, zone string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{corev1.LabelTopologyRegion: region, corev1.LabelTopologyZone: zone},
		},
	}
}

// initMulticlusterServiceDiscovery builds a ServiceEntry registry over a config cluster plus one
// remote cluster, registered later by creating the multicluster secret. configObjects and
// remoteObjects seed each cluster's API server; the returned functions create and delete the secret,
// each waiting for the cluster to appear or disappear. The remote cluster is deleted on cleanup
// whether or not a test does it itself.
func initMulticlusterServiceDiscovery(t test.Failer, networks *meshconfig.MeshNetworks, configObjects, remoteObjects []runtime.Object) (
	model.ConfigStore, *Controller, func(), func(),
) {
	configController := memory.NewController(collections.Pilot, false)

	stop := test.NewStop(t)
	go configController.Run(stop)

	endpoints := model.NewEndpointIndex(model.DisabledCache{})
	xdsUpdater := xdsfake.NewWithDelegate(model.NewEndpointIndexUpdater(endpoints))

	meshcfg := meshwatcher.NewTestWatcher(mesh.DefaultMeshConfig())
	client := kube.NewFakeClient(configObjects...)
	multiclusterController := multicluster.NewController(multicluster.ControllerOptions{
		Client:          client,
		ClusterID:       configClusterID,
		SystemNamespace: systemNamespace,
		MeshConfig:      meshcfg,
		Debugger:        krt.GlobalDebugHandler,
		ClientBuilder: func(kubeConfig []byte, clusterID cluster.ID, configOverrides ...func(*rest.Config)) (kube.Client, error) {
			return kube.NewFakeClient(remoteObjects...), nil
		},
	})

	controller := NewController(
		configController, xdsUpdater, multiclusterController, meshcfg,
		meshwatcher.NewFixedNetworksWatcher(networks),
		testFeatureFlags(),
		WithClusterID(configClusterID),
		WithSystemNamespace(systemNamespace),
	)
	assert.NoError(t, multiclusterController.Run(stop))
	client.RunAndWait(stop)
	go controller.Run(stop)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "remote-secret",
			Namespace: systemNamespace,
			Labels:    map[string]string{multicluster.MultiClusterSecretLabel: "true"},
		},
		Data: map[string][]byte{string(remoteClusterID): []byte("kubeconfig")},
	}
	secrets := client.Kube().CoreV1().Secrets(systemNamespace)
	clusterRegistered := func() bool {
		return slices.ContainsFunc(multiclusterController.Clusters().List(), func(c *multicluster.Cluster) bool {
			return c.ID == remoteClusterID
		})
	}
	addCluster := func() {
		if _, err := secrets.Create(context.Background(), secret, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed creating the multicluster secret: %v", err)
		}
		retry.UntilOrFail(t, clusterRegistered, retry.Timeout(time.Second*30))
	}
	deleteCluster := func() {
		if err := secrets.Delete(context.Background(), secret.Name, metav1.DeleteOptions{}); err != nil {
			if !kerrors.IsNotFound(err) {
				t.Fatalf("failed deleting the multicluster secret: %v", err)
			}
			return
		}
		// Deleting the secret is what stops the remote cluster and shuts its client down. Wait for it:
		// the controller processes the deletion on a queue that the test's stop channel shuts down, so
		// a test that returns without waiting leaves the cluster's informers running.
		retry.UntilOrFail(t, func() bool { return !clusterRegistered() }, retry.Timeout(time.Second*30))
	}
	// Registered after test.NewStop above, so it runs before the stop channel closes.
	t.Cleanup(deleteCluster)

	return configController, controller, addCluster, deleteCluster
}

// TestServiceEntrySelectsRemoteClusterPods covers a ServiceEntry selecting Pods of a remote cluster.
// Those Pods are derived from the remote cluster's own collections - its Pods, its Nodes and the
// network label on its system namespace - rather than being pushed in by the Kubernetes registry that
// owns the cluster.
func TestServiceEntrySelectsRemoteClusterPods(t *testing.T) {
	localPod := testPod(func(p *corev1.Pod) {
		p.Spec.NodeName = "local-node"
	})
	remotePod := testPod(func(p *corev1.Pod) {
		// Deliberately the same namespace/name as the local pod: they are distinct workloads.
		p.Spec.NodeName = "remote-node"
		p.Status.PodIP = "5.6.7.8"
		p.Status.PodIPs = []corev1.PodIP{{IP: "5.6.7.8"}}
	})
	store, sd, addCluster, deleteCluster := initMulticlusterServiceDiscovery(t, nil,
		[]runtime.Object{
			namespaceWithNetwork(systemNamespace, "local-network"),
			nodeWithLocality("local-node", "region1", "zone1"),
			localPod,
		},
		[]runtime.Object{
			namespaceWithNetwork(systemNamespace, "remote-network"),
			nodeWithLocality("remote-node", "region2", "zone2"),
			remotePod,
		},
	)

	se := &config.Config{
		Meta: config.Meta{
			GroupVersionKind:  gvk.ServiceEntry,
			Name:              "selector",
			Namespace:         "ns1",
			CreationTimestamp: GlobalTime,
		},
		Spec: &networking.ServiceEntry{
			Hosts:            []string{"selector.com"},
			Ports:            []*networking.ServicePort{{Number: 444, Name: "http", Protocol: "http"}},
			WorkloadSelector: &networking.WorkloadSelector{Labels: map[string]string{"app": "wle"}},
			Resolution:       networking.ServiceEntry_STATIC,
		},
	}
	createConfigs([]*config.Config{se}, store, t)

	// Reported as "<address>/<cluster>/<network>/<locality>": everything about the endpoint that has to
	// come from the cluster the pod runs in.
	expectPodEndpoints := func(t *testing.T, want []string) {
		t.Helper()
		retry.UntilSuccessOrFail(t, func() error {
			var got []string
			for _, i := range sd.outputs.ServiceInstances.List() {
				if i.Service.Hostname != "selector.com" {
					continue
				}
				ep := i.Endpoint
				got = append(got, fmt.Sprintf("%s/%s/%s/%s", ep.FirstAddressOrNil(), ep.Locality.ClusterID, ep.Network, ep.Locality.Label))
			}
			return assert.Compare(slices.Sort(got), want)
		}, retry.Timeout(time.Second*30))
	}

	expectPodEndpoints(t, []string{"1.2.3.4/config-cluster/local-network/region1/zone1/"})

	addCluster()
	expectPodEndpoints(t, []string{
		"1.2.3.4/config-cluster/local-network/region1/zone1/",
		"5.6.7.8/remote-cluster/remote-network/region2/zone2/",
	})

	// Both workloads are selectable: keying a WorkloadInstance by cluster is what keeps the remote pod
	// from displacing the same-named local one.
	for _, clusterID := range []cluster.ID{configClusterID, remoteClusterID} {
		key := (&model.WorkloadInstance{
			Kind: model.PodKind, Cluster: clusterID, Namespace: "ns1", Name: "pod1",
		}).ResourceName()
		if sd.outputs.AllWorkloads.GetKey(key) == nil {
			t.Fatalf("no workload instance for %v", key)
		}
	}

	// Removing the cluster removes its workloads with it.
	deleteCluster()
	expectPodEndpoints(t, []string{"1.2.3.4/config-cluster/local-network/region1/zone1/"})
}

// TestServiceEntrySelectsRemoteClusterPodsMeshNetworks covers the network of a remote cluster's pods
// coming from the MeshNetworks config: a fromRegistry entry naming that cluster, which the config
// cluster's own MeshNetworkInfo cannot answer.
func TestServiceEntrySelectsRemoteClusterPodsMeshNetworks(t *testing.T) {
	networks := &meshconfig.MeshNetworks{
		Networks: map[string]*meshconfig.Network{
			"local-network": {
				Endpoints: []*meshconfig.Network_NetworkEndpoints{{
					Ne: &meshconfig.Network_NetworkEndpoints_FromRegistry{FromRegistry: string(configClusterID)},
				}},
			},
			"remote-network": {
				Endpoints: []*meshconfig.Network_NetworkEndpoints{{
					Ne: &meshconfig.Network_NetworkEndpoints_FromRegistry{FromRegistry: string(remoteClusterID)},
				}},
			},
		},
	}
	localPod := testPod(func(p *corev1.Pod) {
		p.Spec.NodeName = ""
	})
	remotePod := testPod(func(p *corev1.Pod) {
		p.Spec.NodeName = ""
		p.Status.PodIP = "5.6.7.8"
		p.Status.PodIPs = []corev1.PodIP{{IP: "5.6.7.8"}}
	})
	store, sd, addCluster, _ := initMulticlusterServiceDiscovery(t, networks,
		// Neither system namespace is labelled, so the network can only come from MeshNetworks.
		[]runtime.Object{
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: systemNamespace}},
			localPod,
		},
		[]runtime.Object{
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: systemNamespace}},
			remotePod,
		},
	)

	se := &config.Config{
		Meta: config.Meta{
			GroupVersionKind:  gvk.ServiceEntry,
			Name:              "selector",
			Namespace:         "ns1",
			CreationTimestamp: GlobalTime,
		},
		Spec: &networking.ServiceEntry{
			Hosts:            []string{"selector.com"},
			Ports:            []*networking.ServicePort{{Number: 444, Name: "http", Protocol: "http"}},
			WorkloadSelector: &networking.WorkloadSelector{Labels: map[string]string{"app": "wle"}},
			Resolution:       networking.ServiceEntry_STATIC,
		},
	}
	createConfigs([]*config.Config{se}, store, t)
	addCluster()

	retry.UntilSuccessOrFail(t, func() error {
		var got []string
		for _, i := range sd.outputs.ServiceInstances.List() {
			if i.Service.Hostname != "selector.com" {
				continue
			}
			got = append(got, fmt.Sprintf("%s/%s", i.Endpoint.FirstAddressOrNil(), i.Endpoint.Network))
		}
		return assert.Compare(slices.Sort(got), []string{"1.2.3.4/local-network", "5.6.7.8/remote-network"})
	}, retry.Timeout(time.Second*30))
}
