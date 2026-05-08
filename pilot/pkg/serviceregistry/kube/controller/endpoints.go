package controller

import (
	"strings"

	"istio.io/api/label"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	labelutil "istio.io/istio/pilot/pkg/serviceregistry/util/label"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/config/visibility"
	kubeutil "istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/krt"
	pm "istio.io/istio/pkg/model"
	"istio.io/istio/pkg/network"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/sets"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	mcs "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

type EndpointType string

const (
	EndpointTypePod              EndpointType = "pod"
	EndpointTypeWorkloadInstance EndpointType = "workloadinstance"
	EndpointTypeEndpoint         EndpointType = "endpoint"
)

type ServiceEndpoint struct {
	Type     EndpointType
	Name     string
	Endpoint *model.ServiceInstance
}

func (se ServiceEndpoint) ResourceName() string {
	return string(se.Type) + "/" + se.Endpoint.ResourceName()
}

func (se ServiceEndpoint) Equals(other ServiceEndpoint) bool {
	return se.Type == other.Type &&
		se.Endpoint.Equals(other.Endpoint) &&
		se.Name == other.Name
}

func ServiceEndpoints(
	services krt.Collection[*model.Service],
	endpointSlices krt.Collection[*v1.EndpointSlice],
	pods krt.Collection[*corev1.Pod],
	podsByIP krt.Index[string, *corev1.Pod],
	nodes krt.Collection[*corev1.Node],
	workloadInstancesByNamespace krt.Index[string, *model.WorkloadInstance],
	serviceExports krt.Collection[controllers.Object],
	networkManager *krtNetworkManager,
	trustDomain func(krt.HandlerContext) string,
	domainSuffix string,
	clusterID cluster.ID,
	features Features,
	opts krt.OptionsBuilder,
) krt.Collection[ServiceEndpoint] {
	endpointSlicesByHostname := krt.NewIndex(endpointSlices, "byServiceHostname", func(es *v1.EndpointSlice) []string {
		if _, ok := es.Labels[mcs.LabelServiceName]; ok {
			return nil
		}

		if es.AddressType == v1.AddressTypeFQDN {
			// TODO(https://github.com/istio/istio/issues/34995) support FQDN endpointslice
			return nil
		}

		serviceName, ok := es.Labels[v1.LabelServiceName]
		// This is not a endpointslice for service, ignore
		if !ok {
			return nil
		}

		namespacedName := types.NamespacedName{
			Name:      serviceName,
			Namespace: es.Namespace,
		}

		return slices.Map(hostNamesForNamespacedName(namespacedName, domainSuffix), func(h host.Name) string {
			return h.String()
		})
	})

	return krt.NewManyCollection(services, func(ctx krt.HandlerContext, svc *model.Service) []ServiceEndpoint {
		if svc.Attributes.ExportTo.Contains(visibility.None) {
			return nil
		}

		namespacedName := namespacedNameForService(svc)
		discoverabilityPolicy := endpointDiscoverabilityPolicy(ctx, serviceExports, svc, features)

		endpoints := make([]ServiceEndpoint, 0)
		endpointSlices := endpointSlicesByHostname.Fetch(ctx, svc.Hostname.String())
		found := sets.New[endpointKey]()
		for _, es := range endpointSlices {
			for _, e := range es.Endpoints {
				// Draining tracking is only enabled if persistent sessions is enabled.
				// If we start using them for other features, this can be adjusted.
				healthStatus := endpointHealthStatus(svc, e)
				for _, a := range e.Addresses {
					expectedPod := e.TargetRef != nil && e.TargetRef.Kind == kind.Pod.String()
					var pod *corev1.Pod
					var t EndpointType
					if expectedPod {
						t = EndpointTypePod
						key := types.NamespacedName{Name: e.TargetRef.Name, Namespace: e.TargetRef.Namespace}.String()
						pod = ptr.Flatten(krt.FetchOne(ctx, pods, krt.FilterKey(key)))
						if pod == nil {
							continue
						}
					} else {
						t = EndpointTypeEndpoint
						pods := podsByIP.Fetch(ctx, a, krt.FilterGeneric(func(a any) bool {
							return a.(*corev1.Pod).Namespace == namespacedName.Namespace
						}))
						if len(pods) > 0 {
							pod = pods[0]
						}
					}

					var overrideAddresses []string
					// If not expect a pod, it means this is not an endpointslice not managed by kubernetes.
					// We do not add all pod ips to the istio endpoint.
					if features.EnableDualStack && expectedPod && len(pod.Status.PodIPs) > 1 && len(svc.ClusterVIPs.GetAddressesFor(clusterID)) > 1 {
						if es.AddressType == v1.AddressTypeIPv6 {
							// For endpointslice with targetRef and the pod has dual stack ip.
							// We ignore ipv6 family address to prevent generating duplicate IstioEndpoints.
							continue
						}
						// get the IP addresses for the dual stack pod
						overrideAddresses = slices.Map(pod.Status.PodIPs, func(e corev1.PodIP) string {
							return e.IP
						})
					}

					builder := newEndpointBuilder(ctx, pod, nodes, networkManager, trustDomain, clusterID)
					// EDS and ServiceEntry use name for service port - ADS will need to map to numbers.
					for _, port := range es.Ports {
						var portNum int32
						if port.Port != nil {
							portNum = *port.Port
						}
						var portName string
						if port.Name != nil {
							portName = *port.Name
						}

						istioEndpoint := builder.buildIstioEndpoint(a, portNum, portName, discoverabilityPolicy, healthStatus, svc.SupportsUnhealthyEndpoints())
						if len(overrideAddresses) > 1 {
							istioEndpoint.Addresses = overrideAddresses
						}

						key := endpointKey{istioEndpoint.FirstAddressOrNil(), istioEndpoint.ServicePortName}
						if found.InsertContains(key) {
							continue
						}

						// TODO: do we need different logic for pods?
						port, f := svc.Ports.Get(istioEndpoint.ServicePortName)
						if !f {
							log.Warnf("unexpected state, svc %v missing port %v", svc.Hostname, istioEndpoint.ServicePortName)
							continue
						}
						name := istioEndpoint.WorkloadName
						if pod != nil {
							name = pod.Name
						}
						endpoints = append(endpoints, ServiceEndpoint{
							Type: t,
							Name: name,
							Endpoint: &model.ServiceInstance{
								Service:     svc,
								ServicePort: port,
								Endpoint:    istioEndpoint,
							},
						})
					}
				}
			}
		}

		if !features.EnableK8SServiceSelectWorkloadEntries {
			return endpoints
		}

		if svc.Attributes.LabelSelectors == nil ||
			svc.MeshExternal ||
			len(svc.Ports) == 0 ||
			svc.Resolution != model.ClientSideLB ||
			svc.Attributes.ServiceRegistry != provider.Kubernetes {

			return endpoints
		}

		for _, port := range svc.Ports {
			wis := workloadInstancesByNamespace.Fetch(ctx, svc.Attributes.Namespace, krt.FilterLabel(svc.Attributes.LabelSelectors))
			for _, wi := range wis {
				instance := serviceInstanceFromWorkloadInstance(svc, port, wi)
				if instance != nil {
					endpoints = append(endpoints, ServiceEndpoint{
						Type:     EndpointTypeWorkloadInstance,
						Name:     instance.Endpoint.WorkloadName,
						Endpoint: instance,
					})
				}
			}
		}
		return endpoints
	}, opts.WithName("ServiceEndpoints")...)
}

func (c *KrtController) pushEDS() {
	if c.xdsUpdater == nil {
		return
	}

	shard := model.ShardKeyFromRegistry(c)
	c.outputs.ServiceEndpointsByNsHost.AsCollection().RegisterBatch(func(events []krt.Event[krt.IndexObject[string, ServiceEndpoint]]) {
		c.closeMu.RLock()
		defer c.closeMu.RUnlock()
		if c.closed.Load() {
			return
		}

		configsUpdated := sets.New[model.ConfigKey]()
		for _, e := range events {
			obj := e.Latest()
			ns, host, _ := strings.Cut(obj.Key, "/")
			var svc *model.Service
			if e.Event == controllers.EventDelete {
				c.xdsUpdater.EDSUpdate(shard, host, ns, nil)
				// we don't have obj.Objects on delete events, we lookup the service
				res := c.outputs.JoinedServicesByHostname.Lookup(host)
				if len(res) == 0 {
					continue
				}
				svc = res[0]
			} else {
				instances := slices.Map(obj.Objects, func(i ServiceEndpoint) *model.IstioEndpoint {
					return i.Endpoint.Endpoint
				})
				c.xdsUpdater.EDSUpdate(shard, host, ns, instances)
				svc = obj.Objects[0].Endpoint.Service
			}

			// Service should be the same for every endpoint in the batch since they share the same hostname
			if svc.Resolution != model.Passthrough {
				continue
			}

			supportsOnlyHTTP := true
			for _, p := range svc.Ports {
				if !p.Protocol.IsHTTP() {
					supportsOnlyHTTP = false
					break
				}
			}

			if supportsOnlyHTTP {
				// pure HTTP headless services should not need a full push since they do not
				// require a Listener based on IP: https://github.com/istio/istio/issues/48207
				configsUpdated.Insert(model.ConfigKey{Kind: kind.DNSName, Name: svc.Hostname.String(), Namespace: svc.Attributes.Namespace})
			} else {
				configsUpdated.Insert(model.ConfigKey{Kind: kind.ServiceEntry, Name: svc.Hostname.String(), Namespace: svc.Attributes.Namespace})
			}
		}

		if len(configsUpdated) > 0 {
			c.xdsUpdater.ConfigUpdate(&model.PushRequest{
				ConfigsUpdated: configsUpdated,
				Reason:         model.NewReasonStats(model.HeadlessEndpointUpdate),
			})
		}
	}, false)
}

func (c *KrtController) pushProxy() {
	if c.xdsUpdater == nil {
		return
	}

	c.inputs.Pods.RegisterBatch(func(events []krt.Event[*corev1.Pod]) {
		for _, e := range events {
			if e.Event == controllers.EventDelete {
				continue
			}

			pod := e.Latest()
			if pod.Status.PodIP == "" {
				continue
			}

			if shouldPodBeInEndpoints(pod) && IsPodReady(pod) {
				if e.Event == controllers.EventUpdate {
					// skip updates that are not related to pod readiness or labels
					if IsPodReady(*e.Old) && !labelFilter(*e.Old, pod) {
						continue
					}
				}
				c.xdsUpdater.ProxyUpdate(c.clusterID, pod.Status.PodIP)
			}
		}
	}, false)
}

func (c *KrtController) pushWorkloadInstances(trustDomainGetter func(krt.HandlerContext) string) {
	c.inputs.Pods.RegisterBatch(func(events []krt.Event[*corev1.Pod]) {
		if len(c.handlers.GetWorkloadHandlers()) == 0 {
			return
		}

		for _, e := range events {
			pod := e.Latest()

			ev := model.Event(e.Event)
			switch ev {
			case model.EventAdd:
				if !shouldPodBeInEndpoints(pod) || !IsPodReady(pod) {
					continue
				}
			case model.EventUpdate:
				if !shouldPodBeInEndpoints(pod) || !IsPodReady(pod) {
					if !shouldPodBeInEndpoints(*e.Old) || !IsPodReady(*e.Old) {
						// old pod was not tracked and new pod should not be tracked, no need to notify handlers about deletion
						continue
					}
					ev = model.EventDelete
				}
			case model.EventDelete:
				if !shouldPodBeInEndpoints(pod) || !IsPodReady(pod) {
					// old pod was not tracked, no need to notify handlers about deletion
					continue
				}
			}

			c.fireWorkloadHandlersForPod(pod, trustDomainGetter, ev)
		}
	}, false)
}

func (c *KrtController) fireWorkloadHandlersForPod(pod *corev1.Pod, trustDomainGetter func(krt.HandlerContext) string, event model.Event) {
	// fire instance handles for workload
	epBuilder := newEndpointBuilder(
		nil,
		pod,
		c.inputs.Nodes,
		c.krtNetworkManager,
		trustDomainGetter,
		c.clusterID,
	)
	ep := epBuilder.buildIstioEndpoint(
		pod.Status.PodIP,
		0,
		"",
		model.AlwaysDiscoverable,
		model.Healthy,
		c.features.GlobalSendUnhealthyEndpoints,
	)
	// If pod is dual stack, handle all IPs
	if c.features.EnableDualStack && len(pod.Status.PodIPs) > 1 {
		ep.Addresses = slices.Map(pod.Status.PodIPs, func(e corev1.PodIP) string {
			return e.IP
		})
	}
	workloadInstance := &model.WorkloadInstance{
		Name:      pod.Name,
		Namespace: pod.Namespace,
		Kind:      model.PodKind,
		Endpoint:  ep,
		PortMap:   getPortMap(pod),
	}
	c.handlers.NotifyWorkloadHandlers(workloadInstance, event)
}

func newEndpointBuilder(
	ctx krt.HandlerContext,
	pod *corev1.Pod,
	nodes krt.Collection[*corev1.Node],
	networkManager *krtNetworkManager,
	trustDomain func(krt.HandlerContext) string,
	clusterID cluster.ID,
) *EndpointBuilder {
	var locality, sa, namespace, hostname, subdomain, ip, node string
	var podLabels labels.Instance
	if pod != nil {
		locality = podLocality(ctx, pod, nodes)
		sa = kube.SecureNamingSAN(pod, trustDomain(ctx))
		podLabels = pod.Labels
		namespace = pod.Namespace
		subdomain = pod.Spec.Subdomain
		if subdomain != "" {
			hostname = pod.Spec.Hostname
			if hostname == "" {
				hostname = pod.Name
			}
		}
		ip = pod.Status.PodIP
		node = pod.Spec.NodeName
	}
	dm, _ := kubeutil.GetWorkloadMetaFromPod(pod)
	out := &EndpointBuilder{
		networkFn: func(endpointIP string, labels labels.Instance) network.ID {
			return networkManager.NetworkCtx(ctx, endpointIP, labels)
		},
		serviceAccount: sa,
		locality: model.Locality{
			Label:     locality,
			ClusterID: clusterID,
		},
		tlsMode:      kube.PodTLSMode(pod),
		workloadName: dm.Name,
		namespace:    namespace,
		hostname:     hostname,
		subDomain:    subdomain,
		labels:       podLabels,
		nodeName:     node,
	}
	networkID := out.endpointNetwork(ip)
	out.labels = labelutil.AugmentLabels(podLabels, clusterID, locality, node, networkID)
	return out
}

// getPodLocality retrieves the locality for a pod.
func podLocality(
	ctx krt.HandlerContext,
	pod *corev1.Pod,
	nodes krt.Collection[*corev1.Node],
) string {
	// if pod has `istio-locality` label, skip below ops
	localityLabel := pm.GetLocalityLabel(pod.Labels)
	if localityLabel != "" {
		return pm.SanitizeLocalityLabel(localityLabel)
	}

	// NodeName is set by the scheduler after the pod is created
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#late-initialization
	n := krt.FetchOrList(ctx, nodes, krt.FilterKey(pod.Spec.NodeName))
	if len(n) == 0 {
		if pod.Spec.NodeName != "" {
			log.Warnf("unable to get node %q for pod %q/%q", pod.Spec.NodeName, pod.Namespace, pod.Name)
		}
		return ""
	}
	node := n[0]

	region := getLabelValue(node.ObjectMeta, NodeRegionLabelGA, NodeRegionLabel)
	zone := getLabelValue(node.ObjectMeta, NodeZoneLabelGA, NodeZoneLabel)
	subzone := getLabelValue(node.ObjectMeta, label.TopologySubzone.Name, "")

	if region == "" && zone == "" && subzone == "" {
		return ""
	}

	return region + "/" + zone + "/" + subzone // Format: "%s/%s/%s"
}

func endpointDiscoverabilityPolicy(
	ctx krt.HandlerContext,
	serviceExports krt.Collection[controllers.Object],
	svc *model.Service,
	features Features,
) model.EndpointDiscoverabilityPolicy {
	if !features.EnableMCSServiceDiscovery {
		return model.AlwaysDiscoverable
	}

	if strings.HasSuffix(svc.Hostname.String(), "."+constants.DefaultClusterSetLocalDomain) {
		return checkServiceExport(ctx, serviceExports, namespacedNameForService(svc))
	}

	// MCS cluster.local mode is enabled. Allow endpoints for the cluster.local host to be
	// discoverable only from within the same cluster.
	if features.EnableMCSClusterLocal {
		return model.DiscoverableFromSameCluster
	}

	// If MCS cluster.local mode is not enabled, requests to the cluster.local host are not confined
	// to the same cluster. Use the same discoverability policy as for clusterset.local.
	return checkServiceExport(ctx, serviceExports, namespacedNameForService(svc))
}

func checkServiceExport(
	ctx krt.HandlerContext,
	serviceExports krt.Collection[controllers.Object],
	namespacedName types.NamespacedName,
) model.EndpointDiscoverabilityPolicy {
	// If the service is exported in this cluster, allow the endpoints in this cluster to be discoverable
	// anywhere in the mesh.
	se := krt.FetchOrList(ctx, serviceExports, krt.FilterKey(namespacedName.String()))
	if len(se) > 0 {
		return model.AlwaysDiscoverable
	}

	// Otherwise, endpoints are only discoverable from within the same cluster.
	return model.DiscoverableFromSameCluster
}
