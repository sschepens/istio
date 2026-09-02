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

// Package meshnetworks resolves the network an endpoint belongs to from the MeshNetworks config and
// the system namespace label, as a krt collection.
//
// It exists as its own package because both the Kubernetes registry and the ServiceEntry registry
// need it, and the Kubernetes registry already imports the ServiceEntry one.
package meshnetworks

import (
	"net"

	"github.com/yl2chen/cidranger"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"

	"istio.io/api/label"
	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/kube/krt"
	istiolog "istio.io/istio/pkg/log"
	"istio.io/istio/pkg/network"
	"istio.io/istio/pkg/ptr"
)

var log = istiolog.RegisterScope("meshnetworks", "mesh networks")

// MeshNetworkInfo is everything a registry needs to attribute an endpoint to a network, derived from
// the MeshNetworks config and the topology.istio.io/network label on the system namespace.
type MeshNetworkInfo struct {
	// NetworkFromSystemNamespace is the topology.istio.io/network label on the system namespace.
	NetworkFromSystemNamespace network.ID
	// NetworkFromMeshConfig is the network whose endpoints declare fromRegistry: <this cluster>.
	NetworkFromMeshConfig network.ID
	// RegistryServiceNameGateways maps a gateway's registryServiceName to the partially built
	// NetworkGateways it backs; the addresses are filled in from the Service itself.
	RegistryServiceNameGateways map[host.Name][]model.NetworkGateway
	// Ranger resolves an endpoint IP to a network via the networks' fromCidr entries.
	Ranger cidranger.Ranger

	// meshNetworks is the config every field above (except NetworkFromSystemNamespace) was derived
	// from. It is retained only for equality: cidranger.Ranger has no meaningful comparison, and
	// reflect.DeepEqual on a trie is both expensive and fragile.
	meshNetworks *meshconfig.MeshNetworks
}

func (m MeshNetworkInfo) ResourceName() string { return "MeshNetworkInfo" }

func (m MeshNetworkInfo) Equals(other MeshNetworkInfo) bool {
	return m.NetworkFromSystemNamespace == other.NetworkFromSystemNamespace &&
		proto.Equal(m.meshNetworks, other.meshNetworks)
}

// NetworkForEndpoint returns the network an endpoint belongs to, in the same priority order the
// Kubernetes registry uses: the workload's own label, then the system namespace label, then the
// MeshNetworks config (fromRegistry before fromCidr). Empty if none match.
func (m MeshNetworkInfo) NetworkForEndpoint(endpointIP string, lbls labels.Instance) network.ID {
	// 1. check the pod/workloadEntry label
	if nw := lbls[label.TopologyNetwork.Name]; nw != "" {
		return network.ID(nw)
	}

	// 2. check the system namespace labels
	if m.NetworkFromSystemNamespace != "" {
		return m.NetworkFromSystemNamespace
	}

	// 3. check the meshNetworks config
	if m.NetworkFromMeshConfig != "" {
		return m.NetworkFromMeshConfig
	}

	if m.Ranger != nil {
		ip := net.ParseIP(endpointIP)
		if ip == nil {
			return ""
		}
		entries, err := m.Ranger.ContainingNetworks(ip)
		if err != nil {
			log.Errorf("error getting cidr ranger entry from endpoint ip %s", endpointIP)
			return ""
		}
		if len(entries) > 1 {
			log.Warnf("Found multiple networks CIDRs matching the endpoint IP: %s. Using the first match.", endpointIP)
		}
		if len(entries) > 0 {
			return (entries[0].(namedRangerEntry)).name
		}
	}

	return ""
}

// Network is NetworkForEndpoint for callers inside a krt transformation. Fetching through ctx is what
// makes the calling collection recompute when the mesh networks config or the system namespace label
// changes.
func Network(ctx krt.HandlerContext, info krt.Singleton[MeshNetworkInfo], endpointIP string, lbls labels.Instance) network.ID {
	return ptr.OrEmpty(krt.FetchOne(ctx, info.AsCollection())).NetworkForEndpoint(endpointIP, lbls)
}

// namedRangerEntry for holding network's CIDR and name
type namedRangerEntry struct {
	name    network.ID
	network net.IPNet
}

// Network returns the IPNet for the network
func (n namedRangerEntry) Network() net.IPNet {
	return n.network
}

// LocalMeshNetworkInfo builds the MeshNetworkInfo for clusterID. localNamespaces must be the
// namespaces of the cluster systemNamespace lives in.
func LocalMeshNetworkInfo(
	localNamespaces krt.Collection[*corev1.Namespace],
	meshNetworks meshwatcher.NetworksWatcherCollection,
	systemNamespace string,
	clusterID cluster.ID,
	opts krt.OptionsBuilder,
) krt.Singleton[MeshNetworkInfo] {
	LocalSystemNamespaceNetwork := krt.NewSingleton(func(ctx krt.HandlerContext) *network.ID {
		ns := ptr.Flatten(krt.FetchOne(ctx, localNamespaces, krt.FilterKey(systemNamespace)))
		if ns == nil {
			return nil
		}
		nw, f := ns.Labels[label.TopologyNetwork.Name]
		if !f {
			return nil
		}
		return ptr.Of(network.ID(nw))
	}, opts.WithName("LocalSystemNamespaceNetwork")...)

	return krt.NewSingleton(func(ctx krt.HandlerContext) *MeshNetworkInfo {
		networkFromSystemNamespace := ptr.OrEmpty(krt.FetchOne(ctx, LocalSystemNamespaceNetwork.AsCollection()))
		mni := MeshNetworkInfo{
			NetworkFromSystemNamespace:  networkFromSystemNamespace,
			RegistryServiceNameGateways: make(map[host.Name][]model.NetworkGateway),
		}
		networks := ptr.OrEmpty(krt.FetchOne(ctx, meshNetworks.AsCollection()))
		if networks.MeshNetworks == nil || len(networks.Networks) == 0 {
			return &mni
		}
		mni.meshNetworks = networks.MeshNetworks
		ranger := cidranger.NewPCTrieRanger()
		for id, v := range networks.Networks {
			// track endpoints items from this registry are a part of this network
			fromRegistry := false
			for _, ep := range v.Endpoints {
				if ep.GetFromCidr() != "" {
					_, nw, err := net.ParseCIDR(ep.GetFromCidr())
					if err != nil {
						log.Warnf("unable to parse CIDR %q for network %s", ep.GetFromCidr(), id)
						continue
					}
					rangerEntry := namedRangerEntry{
						name:    network.ID(id),
						network: *nw,
					}
					_ = ranger.Insert(rangerEntry)
				}
				if ep.GetFromRegistry() != "" && cluster.ID(ep.GetFromRegistry()) == clusterID {
					fromRegistry = true
				}
			}

			// fromRegistry field specified this cluster
			if fromRegistry {
				// treat endpoints in this cluster as part of this network
				if mni.NetworkFromMeshConfig != "" {
					log.Warnf("multiple networks specify %s in fromRegistry; endpoints from %s will continue to be treated as part of %s",
						clusterID, clusterID, mni.NetworkFromMeshConfig)
				} else {
					mni.NetworkFromMeshConfig = network.ID(id)
				}

				// services in this registry matching the registryServiceName and port are part of this network
				for _, gw := range v.Gateways {
					if gwSvcName := gw.GetRegistryServiceName(); gwSvcName != "" {
						svc := host.Name(gwSvcName)
						mni.RegistryServiceNameGateways[svc] = append(mni.RegistryServiceNameGateways[svc], model.NetworkGateway{
							Network: network.ID(id),
							Cluster: clusterID,
							Port:    gw.GetPort(),
						})
					}
				}
			}
		}
		mni.Ranger = ranger

		return &mni
	}, opts.WithName("MeshNetworkInfo")...)
}
