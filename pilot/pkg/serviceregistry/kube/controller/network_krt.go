package controller

import (
	"net"
	"strconv"

	"github.com/yl2chen/cidranger"
	"istio.io/api/label"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/config/schema/gvr"
	kubelib "istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/kubetypes"
	"istio.io/istio/pkg/network"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

type krtNetworkManager struct {
	MeshNetworkInfo           krt.Singleton[MeshNetworkInfo]
	NetworkGatewaysCollection krt.Singleton[AggregateGateways]

	// implements NetworkGatewaysWatcher; we need to call c.NotifyGatewayHandlers when our gateways change
	model.NetworkGatewaysHandler
}

func newKrtNetworkManager(
	services krt.Collection[*model.Service],
	localNamespaces krt.Collection[*corev1.Namespace],
	meshNetworks meshwatcher.NetworksWatcherCollection,
	client kubelib.Client,
	systemNamespace string,
	clusterID cluster.ID,
	discoverRemoteGatewayResources bool,
	opts krt.OptionsBuilder,
) *krtNetworkManager {
	localMeshNetworkInfo := LocalMeshNetworkInfo(localNamespaces, meshNetworks, systemNamespace, clusterID, opts)
	networkGateways := NetworkGateways(services, localMeshNetworkInfo, clusterID, client, discoverRemoteGatewayResources, opts)

	n := krtNetworkManager{
		MeshNetworkInfo:           localMeshNetworkInfo,
		NetworkGatewaysCollection: networkGateways,
	}

	networkGateways.AsCollection().RegisterBatch(func(o []krt.Event[AggregateGateways]) {
		n.NotifyGatewayHandlers()
	}, false)

	return &n
}

func (n *krtNetworkManager) Network(ctx krt.HandlerContext, endpointIP string, labels labels.Instance) network.ID {
	// TODO(sschepns): move label checking out of here
	// 1. check the pod/workloadEntry label
	if nw := labels[label.TopologyNetwork.Name]; nw != "" {
		return network.ID(nw)
	}

	// 2. check the system namespace labels
	res := krt.FetchOrList(ctx, n.MeshNetworkInfo.AsCollection())
	if len(res) > 1 {
		panic("FetchOne found for more than 1 item")
	}
	var meshNetworkInfo MeshNetworkInfo
	if len(res) == 1 {
		meshNetworkInfo = res[0]
	}

	if meshNetworkInfo.NetworkFromSystemNamespace != "" {
		return meshNetworkInfo.NetworkFromSystemNamespace
	}

	if meshNetworkInfo.NetworkFromMeshConfig != "" {
		return meshNetworkInfo.NetworkFromMeshConfig
	}

	if meshNetworkInfo.Ranger != nil {
		ip := net.ParseIP(endpointIP)
		if ip == nil {
			return ""
		}
		entries, err := meshNetworkInfo.Ranger.ContainingNetworks(ip)
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

func (n *krtNetworkManager) NetworkGateways() []model.NetworkGateway {
	agg := n.NetworkGatewaysCollection.Get()
	if agg == nil {
		return nil
	}
	return model.SortGateways(agg.UnsortedList())
}

func (n *krtNetworkManager) HasSynced() bool {
	return n.MeshNetworkInfo.AsCollection().HasSynced() &&
		n.NetworkGatewaysCollection.AsCollection().HasSynced()
}

type ServiceNetworkGateways struct {
	ID       string
	Gateways model.NetworkGatewaySet
}

func (s ServiceNetworkGateways) ResourceName() string {
	return s.ID
}

func (s ServiceNetworkGateways) Equals(other ServiceNetworkGateways) bool {
	return s.Gateways.Equals(other.Gateways)
}

type AggregateGateways struct {
	model.NetworkGatewaySet
}

func (a AggregateGateways) ResourceName() string { return "AggregateGateways" }

func (a AggregateGateways) Equals(other AggregateGateways) bool {
	return a.NetworkGatewaySet.Equals(other.NetworkGatewaySet)
}

func NetworkGateways(
	services krt.Collection[*model.Service],
	meshNetworkInfo krt.Singleton[MeshNetworkInfo],
	clusterID cluster.ID,
	client kubelib.Client,
	discoverRemoteGatewayResources bool,
	opts krt.OptionsBuilder,
) krt.Singleton[AggregateGateways] {
	serviceGateways := krt.NewCollection(services, func(ctx krt.HandlerContext, svc *model.Service) *ServiceNetworkGateways {
		addresses := svc.Attributes.ClusterExternalAddresses.GetAddressesFor(clusterID)
		if len(addresses) == 0 {
			return nil
		}

		var gws []model.NetworkGateway
		// We have different types of E/W gateways - those that use mTLS (those are used in sidecar mode when talking cross networks)
		// and those that use double-HBONE (those are used in ambient mode when talking cross cluster). A gateway service may or may
		// not listen on the mTLS (15443, by default) or HBONE (15008) ports, depending on the mode of operation used by the mesh
		// in the remote cluster. We should not use gateways that don't really listen on the right port.
		if nw := svc.Attributes.Labels[label.TopologyNetwork.Name]; nw != "" {
			// TODO label based gateways could support being the gateway for multiple networks
			gws = buildSvcGatways(svc, nw)
		} else {
			meshNetworkInfo := ptr.OrEmpty(krt.FetchOne(ctx, meshNetworkInfo.AsCollection()))
			gws = slices.Clone(meshNetworkInfo.RegistryServiceNameGateways[svc.Hostname])
		}

		if len(gws) == 0 {
			return nil
		}

		// check if we have node port mappings
		nodePortMap := make(map[uint32]uint32)
		if svc.Attributes.ClusterExternalPorts != nil {
			if npm, exists := svc.Attributes.ClusterExternalPorts[clusterID]; exists {
				nodePortMap = npm
			}
		}
		newGateways := sets.NewWithLength[model.NetworkGateway](len(gws) * len(addresses))
		for _, addr := range addresses {
			for _, gw := range gws {
				// what we now have is a service port. If there is a mapping for cluster external ports,
				// look it up and get the node port for the remote port
				if nodePort, exists := nodePortMap[gw.Port]; exists {
					gw.Port = nodePort
				}

				gw.Cluster = clusterID
				gw.Addr = addr
				newGateways.Insert(gw)
			}
		}

		return &ServiceNetworkGateways{
			ID:       string(svc.Hostname),
			Gateways: newGateways,
		}
	}, opts.WithName("ServiceNetworkGateways")...)

	if !features.MultiNetworkGatewayAPI {
		return krt.NewSingleton(func(ctx krt.HandlerContext) *AggregateGateways {
			gatewaySet := sets.New[model.NetworkGateway]()
			res := krt.Fetch(ctx, serviceGateways)
			for _, svcGateways := range res {
				gatewaySet.Merge(svcGateways.Gateways)
			}
			return &AggregateGateways{gatewaySet}
		}, opts.WithName("NetworkGateways")...)
	}

	gatewayClient := kclient.NewDelayedInformer[*gatewayv1.Gateway](client, gvr.KubernetesGateway, kubetypes.StandardInformer, kubetypes.Filter{})
	gatewayClient.Start(opts.Stop())
	gateways := krt.WrapClient(gatewayClient, opts.WithName("informer/Gateways")...)
	gatewayBased := krt.NewCollection(gateways, func(ctx krt.HandlerContext, gw *gatewayv1.Gateway) *ServiceNetworkGateways {
		if nw := gw.GetLabels()[label.TopologyNetwork.Name]; nw == "" {
			return nil
		}

		// Gateway with istio-remote: only discover this from the config cluster
		// this is a way to reference a gateway that lives in a place that this control plane
		// won't have API server access. Nothing will be deployed for these Gateway resources.
		if !discoverRemoteGatewayResources && gw.Spec.GatewayClassName == constants.RemoteGatewayClassName {
			return nil
		}

		autoPassthrough := func(l gatewayv1.Listener) bool {
			return kube.IsAutoPassthrough(gw.GetLabels(), l)
		}

		base := model.NetworkGateway{
			Network: network.ID(gw.GetLabels()[label.TopologyNetwork.Name]),
			Cluster: clusterID,
			ServiceAccount: types.NamespacedName{
				Namespace: gw.Namespace,
				Name:      kube.GatewaySA(gw),
			},
		}
		newGateways := sets.New[model.NetworkGateway]()
		for _, addr := range gw.Spec.Addresses {
			if addr.Type == nil {
				continue
			}
			if addrType := *addr.Type; addrType != gatewayv1.IPAddressType && addrType != gatewayv1.HostnameAddressType {
				continue
			}
			for _, l := range slices.Filter(gw.Spec.Listeners, autoPassthrough) {
				networkGateway := base
				networkGateway.Addr = addr.Value
				networkGateway.Port = uint32(l.Port)
				newGateways.Insert(networkGateway)
			}
			for _, l := range gw.Spec.Listeners {
				if l.Protocol == "HBONE" {
					networkGateway := base
					networkGateway.Addr = addr.Value
					networkGateway.Port = uint32(l.Port)
					networkGateway.HBONEPort = uint32(l.Port)
					newGateways.Insert(networkGateway)
				}
			}
		}

		if len(newGateways) == 0 {
			return nil
		}

		return &ServiceNetworkGateways{
			ID:       string(gw.UID),
			Gateways: newGateways,
		}
	}, opts.WithName("GatewayNetworkGateways")...)

	return krt.NewSingleton(func(ctx krt.HandlerContext) *AggregateGateways {
		gatewaySet := sets.New[model.NetworkGateway]()
		res := krt.Fetch(ctx, serviceGateways)
		for _, svcGateways := range res {
			gatewaySet.Merge(svcGateways.Gateways)
		}

		res = krt.Fetch(ctx, gatewayBased)
		for _, svcGateways := range res {
			gatewaySet.Merge(svcGateways.Gateways)
		}
		return &AggregateGateways{gatewaySet}
	}, opts.WithName("NetworkGateways")...)
}

func buildSvcGatways(svc *model.Service, nw string) []model.NetworkGateway {
	hbonePort := DefaultNetworkGatewayHBONEPort
	gwPort := DefaultNetworkGatewayPort

	if gwPortStr := svc.Attributes.Labels[label.NetworkingGatewayPort.Name]; gwPortStr != "" {
		port, err := strconv.Atoi(gwPortStr)
		if err != nil {
			log.Warnf("could not parse %q for %s on %s/%s; defaulting to %d",
				gwPortStr, label.NetworkingGatewayPort.Name, svc.Attributes.Namespace, svc.Attributes.Name, DefaultNetworkGatewayPort)
		} else {
			gwPort = port
		}
	}

	_, acceptMTLS := svc.Ports.GetByPort(gwPort)
	_, acceptHBONE := svc.Ports.GetByPort(hbonePort)

	if !acceptMTLS && !acceptHBONE {
		log.Warnf("service %s/%s is labeled as gateway, but does not listen neither on port %d nor on port %d",
			svc.Attributes.Namespace, svc.Attributes.Name, gwPort, hbonePort)
		return nil
	}

	if !acceptMTLS {
		gwPort = 0
	}
	if !acceptHBONE {
		hbonePort = 0
	}
	return []model.NetworkGateway{{Port: uint32(gwPort), HBONEPort: uint32(hbonePort), Network: network.ID(nw)}}
}

type MeshNetworkInfo struct {
	NetworkFromSystemNamespace  network.ID
	NetworkFromMeshConfig       network.ID
	RegistryServiceNameGateways map[host.Name][]model.NetworkGateway
	Ranger                      cidranger.Ranger
}

func (m MeshNetworkInfo) ResourceName() string { return "MeshNetworkInfo" }

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
		meshNetworks := ptr.OrEmpty(krt.FetchOne(ctx, meshNetworks.AsCollection()))
		if meshNetworks.MeshNetworks == nil || len(meshNetworks.Networks) == 0 {
			return &mni
		}
		ranger := cidranger.NewPCTrieRanger()
		for id, v := range meshNetworks.Networks {
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
