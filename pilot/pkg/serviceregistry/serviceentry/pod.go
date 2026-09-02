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
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"istio.io/api/label"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
	labelutil "istio.io/istio/pilot/pkg/serviceregistry/util/label"
	"istio.io/istio/pilot/pkg/serviceregistry/util/meshnetworks"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	kubeUtil "istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/krt"
	pm "istio.io/istio/pkg/model"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
)

// convertPodToWorkloadInstance converts a Pod into the WorkloadInstance a ServiceEntry's
// workloadSelector can select.
//
// This is the krt equivalent of what the Kubernetes registry pushes through its workload handlers
// (PodCache.notifyWorkloadHandlers): the EndpointBuilder logic is inlined here rather than reused,
// because building an endpoint from a krt collection means resolving the node, mesh config and
// network through the handler context so the result recomputes when any of them change.
//
// Ports are deliberately left off the endpoint; convertWorkloadInstanceToServiceInstance assigns them
// from the ServiceEntry's ports and this instance's PortMap.
func convertPodToWorkloadInstance(
	ctx krt.HandlerContext,
	pod *v1.Pod,
	nodes krt.Collection[*v1.Node],
	meshConfig krt.Collection[meshwatcher.MeshConfigResource],
	meshNetworkInfo krt.Singleton[meshnetworks.MeshNetworkInfo],
	clusterID cluster.ID,
	flags FeatureFlags,
) *model.WorkloadInstance {
	ip := pod.Status.PodIP
	locality := podLocality(ctx, pod, nodes)
	nodeName := pod.Spec.NodeName

	// The pod's own network label wins, so resolve against the raw labels; AugmentLabels then writes
	// the result back, which is what EndpointBuilder ends up doing across its two steps.
	networkID := meshnetworks.Network(ctx, meshNetworkInfo, ip, pod.Labels)
	lbls := labelutil.AugmentLabels(pod.Labels, clusterID, locality, nodeName, networkID)

	// The fully qualified Pod hostname is "<hostname>.<subdomain>.<pod namespace>.svc.<cluster domain>",
	// so a hostname is only meaningful alongside a subdomain.
	var hostname string
	subDomain := pod.Spec.Subdomain
	if subDomain != "" {
		hostname = pod.Spec.Hostname
		if hostname == "" {
			hostname = pod.Name
		}
	}

	addresses := []string{ip}
	// If pod is dual stack, handle all IPs
	if flags.EnableDualStack && len(pod.Status.PodIPs) > 1 {
		addresses = slices.Map(pod.Status.PodIPs, func(e v1.PodIP) string {
			return e.IP
		})
	}

	mesh := ptr.OrEmpty(krt.FetchOne(ctx, meshConfig))
	dm, _ := kubeUtil.GetWorkloadMetaFromPod(pod)

	return &model.WorkloadInstance{
		Name:      pod.Name,
		Namespace: pod.Namespace,
		Cluster:   clusterID,
		Kind:      model.PodKind,
		Endpoint: &model.IstioEndpoint{
			Labels:         lbls,
			ServiceAccount: kube.SecureNamingSAN(pod, mesh.GetTrustDomain()),
			Locality: model.Locality{
				Label:     locality,
				ClusterID: clusterID,
			},
			TLSMode:                kube.PodTLSMode(pod),
			Addresses:              addresses,
			Network:                networkID,
			WorkloadName:           dm.Name,
			Namespace:              pod.Namespace,
			HostName:               hostname,
			SubDomain:              subDomain,
			DiscoverabilityPolicy:  model.AlwaysDiscoverable,
			HealthStatus:           podHealthStatus(pod),
			SendUnhealthyEndpoints: flags.SendUnhealthyEndpoints,
			NodeName:               nodeName,
		},
		PortMap: getPortMap(pod),
	}
}

// podLocality is Controller.getPodLocality resolved through krt: a change to the pod's node
// recomputes the instances derived from it.
func podLocality(ctx krt.HandlerContext, pod *v1.Pod, nodes krt.Collection[*v1.Node]) string {
	// if pod has `istio-locality` label, skip below ops
	if localityLabel := pm.GetLocalityLabel(pod.Labels); localityLabel != "" {
		return pm.SanitizeLocalityLabel(localityLabel)
	}

	// NodeName is set by the scheduler after the pod is created
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#late-initialization
	if pod.Spec.NodeName == "" {
		return ""
	}
	node := ptr.Flatten(krt.FetchOne(ctx, nodes, krt.FilterKey(pod.Spec.NodeName)))
	if node == nil {
		log.Warnf("unable to get node %q for pod %q/%q", pod.Spec.NodeName, pod.Namespace, pod.Name)
		return ""
	}

	region := getLabelValue(node.ObjectMeta, v1.LabelTopologyRegion, v1.LabelFailureDomainBetaRegion)
	zone := getLabelValue(node.ObjectMeta, v1.LabelTopologyZone, v1.LabelFailureDomainBetaZone)
	subzone := getLabelValue(node.ObjectMeta, label.TopologySubzone.Name, "")

	if region == "" && zone == "" && subzone == "" {
		return ""
	}

	return region + "/" + zone + "/" + subzone // Format: "%s/%s/%s"
}

func getLabelValue(metadata metav1.ObjectMeta, label string, fallBackLabel string) string {
	metaLabels := metadata.GetLabels()
	val := metaLabels[label]
	if val != "" {
		return val
	}

	return metaLabels[fallBackLabel]
}

func getPortMap(pod *v1.Pod) map[string]uint32 {
	pmap := map[string]uint32{}
	for _, c := range pod.Spec.Containers {
		for _, port := range c.Ports {
			if port.Name == "" || port.Protocol != v1.ProtocolTCP {
				continue
			}
			// First port wins, per Kubernetes (https://github.com/kubernetes/kubernetes/issues/54213)
			if _, f := pmap[port.Name]; !f {
				pmap[port.Name] = uint32(port.ContainerPort)
			}
		}
	}
	// Also include ports from native sidecar init containers (restartPolicy=Always).
	for _, c := range pod.Spec.InitContainers {
		if c.RestartPolicy == nil || *c.RestartPolicy != v1.ContainerRestartPolicyAlways {
			continue
		}
		for _, port := range c.Ports {
			if port.Name == "" || port.Protocol != v1.ProtocolTCP {
				continue
			}
			if _, f := pmap[port.Name]; !f {
				pmap[port.Name] = uint32(port.ContainerPort)
			}
		}
	}
	return pmap
}

// podHealthStatus derives the endpoint health from the pod.
//
// Logic from https://github.com/kubernetes/kubernetes/blob/7c873327b679a70337288da62b96dd610858181d/staging/src/k8s.io/endpointslice/utils.go#L37
// Kubernetes has Ready, Serving, and Terminating. We only have a boolean, which is sufficient for our
// cases. This is the same rule the ambient index applies to Pods.
func podHealthStatus(pod *v1.Pod) model.HealthStatus {
	if !kubeUtil.IsPodReady(pod) || pod.DeletionTimestamp != nil {
		return model.UnHealthy
	}
	return model.Healthy
}
