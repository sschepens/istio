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
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/api/label"
	meshconfig "istio.io/api/mesh/v1alpha1"
	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/config/memory"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/util/meshnetworks"
	"istio.io/istio/pilot/pkg/serviceregistry/util/xdsfake"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/kclient/clienttest"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/multicluster"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/retry"
)

const podClusterID = cluster.ID("pod-cluster")

func testPod(modify func(p *corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "ns1",
			Labels:    map[string]string{"app": "wle"},
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "sa1",
			NodeName:           "node1",
			Containers: []corev1.Container{{
				Ports: []corev1.ContainerPort{
					{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
					// Unnamed and non-TCP ports are not part of the port map.
					{ContainerPort: 8081, Protocol: corev1.ProtocolTCP},
					{Name: "udp", ContainerPort: 8082, Protocol: corev1.ProtocolUDP},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			PodIP:      "1.2.3.4",
			PodIPs:     []corev1.PodIP{{IP: "1.2.3.4"}},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	if modify != nil {
		modify(p)
	}
	return p
}

func TestConvertPodToWorkloadInstance(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node1",
			Labels: map[string]string{
				corev1.LabelTopologyRegion: "region1",
				corev1.LabelTopologyZone:   "zone1",
			},
		},
	}

	cases := []struct {
		name        string
		pod         *corev1.Pod
		nodes       []*corev1.Node
		networkInfo meshnetworks.MeshNetworkInfo
		assertion   func(t *testing.T, wi *model.WorkloadInstance)
	}{
		{
			name:  "pod with node locality",
			pod:   testPod(nil),
			nodes: []*corev1.Node{node},
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, wi.Name, "pod1")
				assert.Equal(t, wi.Namespace, "ns1")
				assert.Equal(t, wi.Kind, model.PodKind)
				assert.Equal(t, wi.Endpoint.Addresses, []string{"1.2.3.4"})
				// Ports come from the ServiceEntry, not the pod.
				assert.Equal(t, wi.Endpoint.EndpointPort, uint32(0))
				assert.Equal(t, wi.PortMap, map[string]uint32{"http": 8080})
				assert.Equal(t, wi.Endpoint.ServiceAccount, "spiffe://cluster.local/ns/ns1/sa/sa1")
				assert.Equal(t, wi.Endpoint.WorkloadName, "pod1")
				assert.Equal(t, wi.Endpoint.NodeName, "node1")
				assert.Equal(t, wi.Endpoint.TLSMode, model.DisabledTLSModeLabel)
				assert.Equal(t, wi.Endpoint.HealthStatus, model.Healthy)
				assert.Equal(t, wi.Endpoint.Locality, model.Locality{Label: "region1/zone1/", ClusterID: podClusterID})
				assert.Equal(t, wi.Endpoint.Labels["topology.kubernetes.io/region"], "region1")
				assert.Equal(t, wi.Endpoint.Labels[label.TopologyCluster.Name], podClusterID.String())
			},
		},
		{
			name: "istio-locality label wins over the node",
			pod: testPod(func(p *corev1.Pod) {
				p.Labels["istio-locality"] = "region2.zone2.subzone2"
			}),
			nodes: []*corev1.Node{node},
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, wi.Endpoint.Locality.Label, "region2/zone2/subzone2")
			},
		},
		{
			name: "unscheduled pod has no locality",
			pod: testPod(func(p *corev1.Pod) {
				p.Spec.NodeName = ""
			}),
			nodes: []*corev1.Node{node},
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, wi.Endpoint.Locality.Label, "")
				assert.Equal(t, wi.Endpoint.NodeName, "")
			},
		},
		{
			name:  "unknown node has no locality",
			pod:   testPod(nil),
			nodes: nil,
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, wi.Endpoint.Locality.Label, "")
			},
		},
		{
			name: "network from the pod label",
			pod: testPod(func(p *corev1.Pod) {
				p.Labels[label.TopologyNetwork.Name] = "pod-network"
			}),
			networkInfo: meshnetworks.MeshNetworkInfo{NetworkFromSystemNamespace: "ns-network"},
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, string(wi.Endpoint.Network), "pod-network")
				assert.Equal(t, wi.Endpoint.Labels[label.TopologyNetwork.Name], "pod-network")
			},
		},
		{
			name:        "network from the system namespace",
			pod:         testPod(nil),
			networkInfo: meshnetworks.MeshNetworkInfo{NetworkFromSystemNamespace: "ns-network"},
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, string(wi.Endpoint.Network), "ns-network")
				assert.Equal(t, wi.Endpoint.Labels[label.TopologyNetwork.Name], "ns-network")
			},
		},
		{
			name: "hostname is only set alongside a subdomain",
			pod: testPod(func(p *corev1.Pod) {
				p.Spec.Hostname = "host"
			}),
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, wi.Endpoint.HostName, "")
				assert.Equal(t, wi.Endpoint.SubDomain, "")
			},
		},
		{
			name: "hostname defaults to the pod name",
			pod: testPod(func(p *corev1.Pod) {
				p.Spec.Subdomain = "sub"
			}),
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, wi.Endpoint.HostName, "pod1")
				assert.Equal(t, wi.Endpoint.SubDomain, "sub")
			},
		},
		{
			name: "mutual TLS from the pod label",
			pod: testPod(func(p *corev1.Pod) {
				p.Labels[label.SecurityTlsMode.Name] = model.IstioMutualTLSModeLabel
			}),
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, wi.Endpoint.TLSMode, model.IstioMutualTLSModeLabel)
			},
		},
		{
			name: "unready pod is unhealthy",
			pod: testPod(func(p *corev1.Pod) {
				p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
			}),
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, wi.Endpoint.HealthStatus, model.UnHealthy)
			},
		},
		{
			name: "pod with no ready condition is unhealthy",
			pod: testPod(func(p *corev1.Pod) {
				p.Status.Conditions = nil
			}),
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, wi.Endpoint.HealthStatus, model.UnHealthy)
			},
		},
		{
			name: "terminating pod is unhealthy but still an endpoint",
			pod: testPod(func(p *corev1.Pod) {
				now := metav1.Now()
				p.DeletionTimestamp = &now
			}),
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, wi.Endpoint.HealthStatus, model.UnHealthy)
				assert.Equal(t, wi.Endpoint.Addresses, []string{"1.2.3.4"})
			},
		},
		{
			name: "native sidecar init container ports are in the port map",
			pod: testPod(func(p *corev1.Pod) {
				always := corev1.ContainerRestartPolicyAlways
				p.Spec.InitContainers = []corev1.Container{{
					RestartPolicy: &always,
					Ports:         []corev1.ContainerPort{{Name: "sidecar", ContainerPort: 15020, Protocol: corev1.ProtocolTCP}},
				}}
			}),
			assertion: func(t *testing.T, wi *model.WorkloadInstance) {
				assert.Equal(t, wi.PortMap, map[string]uint32{"http": 8080, "sidecar": 15020})
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			stop := test.NewStop(t)
			nodes := krt.NewStaticCollection(nil, tt.nodes, krt.WithStop(stop), krt.WithDebugging(krt.GlobalDebugHandler))
			networkInfo := krt.NewStatic(&tt.networkInfo, true)
			meshConfig := meshwatcher.NewTestWatcher(mesh.DefaultMeshConfig()).AsCollection()

			wi := convertPodToWorkloadInstance(krt.TestingDummyContext{}, tt.pod, nodes, meshConfig, networkInfo, podClusterID, testFeatureFlags())
			tt.assertion(t, wi)
		})
	}
}

func TestConvertPodToWorkloadInstanceSendUnhealthyEndpoints(t *testing.T) {
	for _, sendUnhealthy := range []bool{true, false} {
		t.Run(fmt.Sprint(sendUnhealthy), func(t *testing.T) {
			flags := testFeatureFlags()
			flags.SendUnhealthyEndpoints = sendUnhealthy

			wi := convertTestPod(t, testPod(nil), flags)
			assert.Equal(t, wi.Endpoint.SendUnhealthyEndpoints, sendUnhealthy)
		})
	}
}

func TestConvertPodToWorkloadInstanceDualStack(t *testing.T) {
	pod := testPod(func(p *corev1.Pod) {
		p.Status.PodIPs = []corev1.PodIP{{IP: "1.2.3.4"}, {IP: "2001:db8::1"}}
	})

	flags := testFeatureFlags()
	flags.EnableDualStack = false
	assert.Equal(t, convertTestPod(t, pod, flags).Endpoint.Addresses, []string{"1.2.3.4"})

	flags.EnableDualStack = true
	assert.Equal(t, convertTestPod(t, pod, flags).Endpoint.Addresses, []string{"1.2.3.4", "2001:db8::1"})
}

// convertTestPod converts a pod with no nodes and a default mesh config, for the cases that only care
// about the feature flags.
func convertTestPod(t test.Failer, pod *corev1.Pod, flags FeatureFlags) *model.WorkloadInstance {
	stop := test.NewStop(t)
	return convertPodToWorkloadInstance(
		krt.TestingDummyContext{},
		pod,
		krt.NewStaticCollection[*corev1.Node](nil, nil, krt.WithStop(stop), krt.WithDebugging(krt.GlobalDebugHandler)),
		meshwatcher.NewTestWatcher(mesh.DefaultMeshConfig()).AsCollection(),
		krt.NewStatic(&meshnetworks.MeshNetworkInfo{}, true),
		podClusterID,
		flags,
	)
}

// initPodServiceDiscovery is initServiceDiscoveryWithOpts plus access to the config cluster's client,
// so a test can drive the Pods the ServiceEntry controller derives its workloads from.
func initPodServiceDiscovery(t test.Failer, networks *meshconfig.MeshNetworks) (
	model.ConfigStore, *Controller, kube.CLIClient,
) {
	configController := memory.NewController(collections.Pilot, false)

	stop := test.NewStop(t)
	go configController.Run(stop)

	endpoints := model.NewEndpointIndex(model.DisabledCache{})
	xdsUpdater := xdsfake.NewWithDelegate(model.NewEndpointIndexUpdater(endpoints))

	meshcfg := meshwatcher.NewTestWatcher(mesh.DefaultMeshConfig())
	client := kube.NewFakeClient()
	multiclusterController := multicluster.NewController(multicluster.ControllerOptions{
		Client:          client,
		ClusterID:       client.ClusterID(),
		SystemNamespace: meshcfg.Mesh().RootNamespace,
		MeshConfig:      meshcfg,
		Debugger:        krt.GlobalDebugHandler,
	})

	controller := NewController(
		configController, xdsUpdater, multiclusterController, meshcfg,
		meshwatcher.NewFixedNetworksWatcher(networks),
		testFeatureFlags(),
		WithClusterID(client.ClusterID()),
		WithSystemNamespace(meshcfg.Mesh().RootNamespace),
	)
	assert.NoError(t, multiclusterController.Run(stop))
	client.RunAndWait(stop)
	go controller.Run(stop)

	return configController, controller, client
}

func TestServiceEntrySelectsPods(t *testing.T) {
	store, sd, client := initPodServiceDiscovery(t, nil)
	pods := clienttest.NewWriter[*corev1.Pod](t, client)
	clienttest.NewWriter[*corev1.Node](t, client).Create(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node1",
			Labels: map[string]string{corev1.LabelTopologyRegion: "region1", corev1.LabelTopologyZone: "zone1"},
		},
	})

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

	// Reported as "<address>/<health>" so that the tests can tell "gone" apart from "still an
	// endpoint, but unhealthy".
	expectPodEndpoints := func(t *testing.T, want []string) {
		t.Helper()
		retry.UntilSuccessOrFail(t, func() error {
			var got []string
			for _, i := range sd.outputs.ServiceInstances.List() {
				if i.Service.Hostname == "selector.com" {
					health := "healthy"
					if i.Endpoint.HealthStatus == model.UnHealthy {
						health = "unhealthy"
					}
					got = append(got, i.Endpoint.FirstAddressOrNil()+"/"+health)
				}
			}
			return assert.Compare(got, want)
		}, retry.Timeout(time.Second*5))
	}

	t.Run("ready pod is selected", func(t *testing.T) {
		pods.CreateOrUpdateStatus(testPod(nil))
		expectPodEndpoints(t, []string{"1.2.3.4/healthy"})
		retry.UntilSuccessOrFail(t, func() error {
			key := (&model.WorkloadInstance{
				Kind: model.PodKind, Namespace: "ns1", Name: "pod1",
			}).ResourceName()
			wi := sd.outputs.AllWorkloads.GetKey(key)
			if wi == nil {
				return fmt.Errorf("no workload instance for ns1/pod1")
			}
			return assert.Compare((*wi).Endpoint.Locality.Label, "region1/zone1/")
		}, retry.Timeout(time.Second*5))
	})

	t.Run("pod that goes unready stays, marked unhealthy", func(t *testing.T) {
		pods.CreateOrUpdateStatus(testPod(func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		}))
		expectPodEndpoints(t, []string{"1.2.3.4/unhealthy"})
		pods.CreateOrUpdateStatus(testPod(nil))
		expectPodEndpoints(t, []string{"1.2.3.4/healthy"})
	})

	t.Run("terminating pod stays, marked unhealthy", func(t *testing.T) {
		pods.CreateOrUpdateStatus(testPod(nil))
		expectPodEndpoints(t, []string{"1.2.3.4/healthy"})
		now := metav1.Now()
		pods.CreateOrUpdateStatus(testPod(func(p *corev1.Pod) {
			p.DeletionTimestamp = &now
		}))
		expectPodEndpoints(t, []string{"1.2.3.4/unhealthy"})
	})

	t.Run("pod that reaches a terminal phase is dropped", func(t *testing.T) {
		pods.CreateOrUpdateStatus(testPod(nil))
		expectPodEndpoints(t, []string{"1.2.3.4/healthy"})
		pods.CreateOrUpdateStatus(testPod(func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodSucceeded
		}))
		expectPodEndpoints(t, nil)
	})

	t.Run("pod that loses its IP is dropped", func(t *testing.T) {
		pods.CreateOrUpdateStatus(testPod(nil))
		expectPodEndpoints(t, []string{"1.2.3.4/healthy"})
		pods.CreateOrUpdateStatus(testPod(func(p *corev1.Pod) {
			p.Status.PodIP = ""
			p.Status.PodIPs = nil
		}))
		expectPodEndpoints(t, nil)
	})

	t.Run("pod that stops matching the selector is dropped", func(t *testing.T) {
		pods.CreateOrUpdateStatus(testPod(nil))
		expectPodEndpoints(t, []string{"1.2.3.4/healthy"})
		pods.CreateOrUpdateStatus(testPod(func(p *corev1.Pod) {
			p.Labels["app"] = "other"
		}))
		expectPodEndpoints(t, nil)
	})

	t.Run("deleted pod is dropped", func(t *testing.T) {
		pods.CreateOrUpdateStatus(testPod(nil))
		expectPodEndpoints(t, []string{"1.2.3.4/healthy"})
		pods.Delete("pod1", "ns1")
		expectPodEndpoints(t, nil)
	})
}
