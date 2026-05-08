package controller

import (
	"fmt"
	"sync"
	"time"

	"go.uber.org/atomic"
	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
	kubesr "istio.io/istio/pilot/pkg/serviceregistry/kube"
	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	labelutil "istio.io/istio/pilot/pkg/serviceregistry/util/label"
	"istio.io/istio/pilot/pkg/serviceregistry/util/workloadinstances"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/labels"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/config/schema/gvr"
	kubelib "istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/kubetypes"
	"istio.io/istio/pkg/maps"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
	v1 "k8s.io/api/core/v1"
	discovery "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	mcs "sigs.k8s.io/mcs-api/pkg/apis/v1alpha1"
)

var _ serviceregistry.Instance = &KrtController{}

type Inputs struct {
	Pods                      krt.Collection[*v1.Pod]
	PodsByIP                  krt.Index[string, *v1.Pod]
	Nodes                     krt.Collection[*v1.Node]
	Services                  krt.Collection[*v1.Service]
	Namespaces                krt.Collection[*v1.Namespace]
	EndpointSlices            krt.Collection[*discovery.EndpointSlice]
	EndpointSlicesByNamespace krt.Index[string, *discovery.EndpointSlice]
	Gateways                  krt.Collection[*gatewayv1.Gateway]

	WorkloadInstances            krt.StaticCollection[*model.WorkloadInstance]
	WorkloadInstancesByNamespace krt.Index[string, *model.WorkloadInstance]
	WorkloadInstancesByIP        krt.Index[string, *model.WorkloadInstance]

	ServiceExports krt.Collection[controllers.Object]
	ServiceImports krt.Collection[controllers.Object]
}

type Outputs struct {
	MCSServices               krt.Collection[*model.Service]
	JoinedServices            krt.Collection[*model.Service]
	JoinedServicesByNamespace krt.Index[string, *model.Service]
	JoinedServicesByHostname  krt.Index[string, *model.Service]
	ServiceEndpoints          krt.Collection[ServiceEndpoint]
	ServiceEndpointsByNsHost  krt.Index[string, ServiceEndpoint]
}

type Features struct {
	EnableK8SServiceSelectWorkloadEntries bool
	EnableProxyFindPodByIP                bool
	EnableDualStack                       bool
	GlobalSendUnhealthyEndpoints          bool
	EnableMCSServiceDiscovery             bool
	EnableMCSClusterLocal                 bool
	MultiNetworkGatewayAPI                bool
	EnableMCSHost                         bool
}

type KrtController struct {
	opts Options

	client kubelib.Client

	features Features

	xdsUpdater model.XDSUpdater

	handlers model.ControllerHandlers

	LocalMeshWatcher         meshwatcher.WatcherCollection
	ConfigClusterMeshWatcher meshwatcher.WatcherCollection

	*krtNetworkManager

	outputs Outputs
	inputs  Inputs

	clusterID cluster.ID

	stop chan struct{}

	// initialSyncTimedout is set to true after performing an initial processing timed out.
	initialSyncTimedout *atomic.Bool
	// closed is used to avoid racing EDS Updates with Shard removal on controller shutdown.
	closed  *atomic.Bool
	closeMu *sync.RWMutex

	networksHandlerRegistration *mesh.WatcherHandlerRegistration
}

type ClusterCollections interface {
	Namespaces() krt.Collection[*v1.Namespace]
	Pods() krt.Collection[*v1.Pod]
	Services() krt.Collection[*v1.Service]
	EndpointSlices() krt.Collection[*discovery.EndpointSlice]
	Nodes() krt.Collection[*v1.Node]
	Gateways() krt.Collection[*gatewayv1.Gateway]
}

func NewKrtController(client kubelib.Client, cluster ClusterCollections, features Features, opts Options) *KrtController {
	stop := make(chan struct{})
	kopts := krt.NewOptionsBuilder(stop, "KubeServiceRegistry", opts.KrtDebugger)
	inputs := Inputs{
		Pods: cluster.Pods(),
		PodsByIP: krt.NewIndex(cluster.Pods(), "ip", func(pod *v1.Pod) []string {
			if pod.Status.PodIP == "" {
				return nil
			}

			return []string{pod.Status.PodIP}
		}),
		Nodes:                     cluster.Nodes(),
		Services:                  cluster.Services(),
		Namespaces:                cluster.Namespaces(),
		EndpointSlices:            cluster.EndpointSlices(),
		EndpointSlicesByNamespace: krt.NewNamespaceIndex(cluster.EndpointSlices()),
		Gateways:                  cluster.Gateways(),
	}

	if features.EnableMCSHost {
		inputs.ServiceImports = krt.WrapClient(kclient.NewDelayedInformer[controllers.Object](client, gvr.ServiceImport, kubetypes.DynamicInformer, kclient.Filter{
			ObjectFilter: client.ObjectFilter(),
		}), kopts.WithName("informer/ServiceImports")...)
		inputs.ServiceExports = krt.WrapClient(kclient.NewDelayedInformer[controllers.Object](client, gvr.ServiceExport, kubetypes.DynamicInformer, kclient.Filter{
			ObjectFilter: client.ObjectFilter(),
		}), kopts.WithName("informer/ServiceExports")...)
	}

	if features.EnableK8SServiceSelectWorkloadEntries {
		inputs.WorkloadInstances = krt.NewStaticCollection[*model.WorkloadInstance](nil, nil, kopts.WithName("ExternalWorkloads")...)
		inputs.WorkloadInstancesByNamespace = krt.NewNamespaceIndex(inputs.WorkloadInstances)
		inputs.WorkloadInstancesByIP = krt.NewIndex(inputs.WorkloadInstances, "ip", func(wi *model.WorkloadInstance) []string {
			if len(wi.Endpoint.Addresses) == 0 {
				return nil
			}
			return wi.Endpoint.Addresses
		})
	}

	c := KrtController{
		opts:                     opts,
		client:                   client,
		features:                 features,
		xdsUpdater:               opts.XDSUpdater,
		stop:                     stop,
		initialSyncTimedout:      atomic.NewBool(false),
		closed:                   atomic.NewBool(false),
		closeMu:                  &sync.RWMutex{},
		LocalMeshWatcher:         opts.MeshWatcher,
		ConfigClusterMeshWatcher: opts.ConfigClusterMeshWatcher,
		clusterID:                opts.ClusterID,
		inputs:                   inputs,
	}

	trustDomainGetter := func(ctx krt.HandlerContext) string {
		meshes := krt.FetchOrList(ctx, c.LocalMeshWatcher.AsCollection())
		if len(meshes) == 0 {
			return ""
		}

		if td := meshes[0].TrustDomain; td != "" {
			return td
		}

		if c.ConfigClusterMeshWatcher != nil {
			meshes = krt.FetchOrList(ctx, c.ConfigClusterMeshWatcher.AsCollection())
			if len(meshes) == 0 {
				return ""
			}
			return meshes[0].TrustDomain
		}

		return ""
	}
	c.buildServiceCollections(trustDomainGetter, kopts)
	c.krtNetworkManager = newKrtNetworkManager(
		c.outputs.JoinedServices,
		c.inputs.Namespaces,
		c.inputs.Gateways,
		c.opts.MeshNetworksWatcher,
		c.opts.XDSUpdater,
		c.opts.SystemNamespace,
		c.opts.ClusterID,
		c.opts.ConfigCluster,
		c.features,
		kopts,
	)
	c.buildEndpointCollections(trustDomainGetter, kopts)

	c.pushServices()
	c.pushEDS()
	c.pushProxy()
	c.pushWorkloadInstances(trustDomainGetter)

	if c.opts.MeshNetworksWatcher != nil {
		c.networksHandlerRegistration = c.opts.MeshNetworksWatcher.AddNetworksHandler(func() {
			pods := c.inputs.Pods.List()
			for _, pod := range pods {
				c.fireWorkloadHandlersForPod(pod, trustDomainGetter, model.EventAdd)
			}

			c.NotifyGatewayHandlers()
		})
	}

	c.krtNetworkManager.MeshNetworkInfo.AsCollection().RegisterBatch(func(events []krt.Event[MeshNetworkInfo]) {
		shouldRecompute := false
		for _, e := range events {
			if e.Event == controllers.EventUpdate && e.Old.NetworkFromSystemNamespace != e.New.NetworkFromSystemNamespace {
				shouldRecompute = true
			}
		}

		if !shouldRecompute {
			return
		}

		pods := c.inputs.Pods.List()
		for _, pod := range pods {
			c.fireWorkloadHandlersForPod(pod, trustDomainGetter, model.EventAdd)
		}

		c.NotifyGatewayHandlers()
	}, false)

	return &c
}

func (c *KrtController) buildServiceCollections(trustDomainGetter func(ctx krt.HandlerContext) string, opts krt.OptionsBuilder) {
	nativeServices := Services(
		c.inputs.Services,
		c.inputs.Nodes,
		c.inputs.Namespaces,
		c.opts.DomainSuffix,
		c.clusterID,
		trustDomainGetter,
		opts,
	)

	serviceCollections := []krt.Collection[*model.Service]{nativeServices}

	if c.features.EnableMCSHost {
		c.outputs.MCSServices = MCSServices(
			c.inputs.ServiceImports,
			c.opts.MeshServiceController,
			c.opts.DomainSuffix,
			c.clusterID,
			opts,
		)
		serviceCollections = append(serviceCollections, c.outputs.MCSServices)
	}

	c.outputs.JoinedServices = krt.JoinCollection(
		serviceCollections,
		// services and mcs services use different hostnames, we can avoid checking overlaps
		opts.With(krt.WithJoinUnchecked(), krt.WithName("JoinedServices"))...,
	)

	c.outputs.JoinedServicesByNamespace = krt.NewNamespaceIndex(c.outputs.JoinedServices)

	c.outputs.JoinedServicesByHostname = krt.NewIndex(c.outputs.JoinedServices, "hostname", func(svc *model.Service) []string {
		return []string{string(svc.Hostname)}
	})
}

func (c *KrtController) buildEndpointCollections(trustDomainGetter func(ctx krt.HandlerContext) string, opts krt.OptionsBuilder) {
	c.outputs.ServiceEndpoints = ServiceEndpoints(
		c.outputs.JoinedServices,
		c.inputs.EndpointSlices,
		c.inputs.Pods,
		c.inputs.PodsByIP,
		c.inputs.Nodes,
		c.inputs.WorkloadInstancesByNamespace,
		c.inputs.ServiceExports,
		c.krtNetworkManager,
		trustDomainGetter,
		c.opts.DomainSuffix,
		c.clusterID,
		c.features,
		opts,
	)

	c.outputs.ServiceEndpointsByNsHost = krt.NewIndex(c.outputs.ServiceEndpoints, "nsHost", func(se ServiceEndpoint) []string {
		return []string{se.Endpoint.Service.Attributes.Namespace + "/" + string(se.Endpoint.Service.Hostname)}
	})
}

func (c *KrtController) Provider() provider.ID {
	return provider.Kubernetes
}

func (c *KrtController) Cluster() cluster.ID {
	return c.opts.ClusterID
}

func (c *KrtController) Services() []*model.Service {
	return slices.SortFunc(c.outputs.JoinedServices.List(), func(a, b *model.Service) int {
		if a.Hostname < b.Hostname {
			return -1
		}
		if a.Hostname > b.Hostname {
			return 1
		}

		return 0
	})
}

func (c *KrtController) GetService(hostname host.Name) *model.Service {
	if res := c.outputs.JoinedServicesByHostname.Lookup(string(hostname)); len(res) > 0 {
		return res[0]
	}

	return nil
}

// isControllerForProxy should be used for proxies assumed to be in the kube cluster for this controller. Workload Entries
// may not necessarily pass this check, but we still want to allow kube services to select workload instances.
func (c *KrtController) isControllerForProxy(proxy *model.Proxy) bool {
	return proxy.Metadata.ClusterID == "" || proxy.Metadata.ClusterID == c.Cluster()
}

func (c *KrtController) GetProxyServiceTargets(proxy *model.Proxy) []model.ServiceTarget {
	if !c.isControllerForProxy(proxy) {
		log.Errorf("proxy is in cluster %v, but controller is for cluster %v", proxy.Metadata.ClusterID, c.Cluster())
		proxyNoSvcTargetWrongCluster.Increment()
		return nil
	}

	if len(proxy.IPAddresses) > 0 {
		if c.features.EnableK8SServiceSelectWorkloadEntries {
			// look up for a WorkloadEntry; if there are multiple WorkloadEntry(s)
			// with the same IP, choose one deterministically
			if wi := c.findWorkloadInstanceForProxy(proxy); wi != nil {
				return c.serviceTargetsFromWorkloadInstance(wi)
			}
		}

		if !proxy.IsVM() {
			if pod := c.podForProxy(proxy); pod != nil {
				// 1. find proxy service by label selector, if not any, there may exist headless service without selector
				// failover to 2
				if targets := c.serviceTargetsFromPod(pod); len(targets) > 0 {
					return targets
				}

				// 2. Headless service without selector
				if targets := c.serviceTargetsFromEndpointSlices(proxy, pod); len(targets) > 0 {
					return targets
				}

				proxyNoSvcTargetMissingService.Increment()
			}
		}

		// 3. The pod is not present when this is called
		// due to eventual consistency issues. However, we have a lot of information about the pod from the proxy
		// metadata already. Because of this, we can still get most of the information we need.
		// If we cannot accurately construct ServiceEndpoints from just the metadata, this will return an error and we can
		// attempt to read the real pod.
		out, err := c.getProxyServiceTargetsFromMetadata(proxy)
		if err != nil {
			log.Errorf("failed to get proxy service targets from metadata: %v", err)
		}
		if len(out) == 0 {
			proxyNoSvcTargetFromMetadata.Increment()
		}
		return out
	}

	return nil
}

func (c *KrtController) podForProxy(proxy *model.Proxy) *v1.Pod {
	key := podKeyByProxy(proxy)
	if pod := ptr.Flatten(c.inputs.Pods.GetKey(key.String())); pod != nil {
		return pod
	}

	if c.features.EnableProxyFindPodByIP {
		if pods := c.inputs.PodsByIP.Lookup(proxy.IPAddresses[0]); len(pods) > 0 {
			if len(pods) == 1 {
				return pods[0]
			}

			log.Errorf("unexpected: found multiple pods for proxy %v (%v)", proxy.ID, proxy.IPAddresses[0])
			for _, p := range pods {
				// At least filter out wrong namespaces...
				if proxy.ConfigNamespace == p.Namespace {
					return p
				}
			}
		}
	}

	return nil
}

func (c *KrtController) findWorkloadInstanceForProxy(proxy *model.Proxy) *model.WorkloadInstance {
	proxyIP := proxy.IPAddresses[0]
	instances := c.inputs.WorkloadInstancesByIP.Lookup(proxyIP)
	if len(instances) == 0 {
		return nil
	}

	if len(instances) == 1 {
		return instances[0]
	}

	proxyName := workloadinstances.InstanceNameForProxy(proxy)
	if proxyName.Name != "" {
		// try to find workload instance with the same name as proxy
		for _, wi := range instances {
			if wi.Name == proxyName.Name && wi.Namespace == proxyName.Namespace {
				return wi
			}
		}
	}

	// try to find workload instance in the same namespace as proxy
	for _, wi := range instances {
		if wi.Namespace == proxy.ConfigNamespace {
			return wi
		}
	}

	// fall back to choosing one of the workload instances

	// NOTE: for the sake of backwards compatibility, we don't enforce
	//       instance.Namespace == proxy.ConfigNamespace
	return instances[0]
}

func (c *KrtController) serviceTargetsFromWorkloadInstance(si *model.WorkloadInstance) []model.ServiceTarget {
	out := make([]model.ServiceTarget, 0)
	allServices := c.outputs.JoinedServicesByNamespace.Lookup(si.Namespace)
	for _, service := range allServices {
		// Note that this cannot be an external service because k8s external services do not have label selectors.
		if service == nil || service.Resolution != model.ClientSideLB {
			// may be a headless service
			continue
		}

		if !labels.Instance(service.Attributes.LabelSelectors).Match(si.Endpoint.Labels) {
			continue
		}

		for _, servicePort := range service.Ports {
			if servicePort.Protocol == protocol.UDP {
				continue
			}

			instance := serviceInstanceFromWorkloadInstance(service, servicePort, si)
			if instance != nil {
				out = append(out, model.ServiceInstanceToTarget(instance))
			}
		}
	}
	return out
}

func (c *KrtController) serviceTargetsFromPod(pod *v1.Pod) []model.ServiceTarget {
	if allServices := c.outputs.JoinedServicesByNamespace.Lookup(pod.Namespace); len(allServices) > 0 {
		out := make([]model.ServiceTarget, 0)
		for _, svc := range allServices {
			// Note that this cannot be an external service because k8s external services do not have label selectors.
			if svc == nil || svc.Resolution != model.ClientSideLB {
				// may be a headless service
				continue
			}

			if !labels.Instance(svc.Attributes.LabelSelectors).Match(pod.Labels) {
				continue
			}

			out = append(out, c.serviceTargetsByPod(pod, svc)...)
		}
		return out
	}

	return nil
}

// serviceTargetsFromEndpointSlices is used to find headless services without selectors that select the pod as an endpoint.
// This replicates the logic with which we extract endpoints from EndpointSlices in ServiceEndpoints collection,
// since we cannot rely on the collection being synced when this is called.
func (c *KrtController) serviceTargetsFromEndpointSlices(proxy *model.Proxy, pod *v1.Pod) []model.ServiceTarget {
	if eps := c.inputs.EndpointSlicesByNamespace.Lookup(pod.Namespace); len(eps) > 0 {
		out := make([]model.ServiceTarget, 0)
		for _, es := range eps {
			if _, ok := es.Labels[mcs.LabelServiceName]; ok {
				continue
			}

			if es.AddressType == discovery.AddressTypeFQDN {
				// TODO(https://github.com/istio/istio/issues/34995) support FQDN endpointslice
				continue
			}

			serviceName, ok := es.Labels[discovery.LabelServiceName]
			// This is not a endpointslice for service, ignore
			if !ok {
				continue
			}

			for _, ep := range es.Endpoints {
				if !slices.Contains(ep.Addresses, proxy.IPAddresses[0]) {
					continue
				}

				hostname := kube.ServiceHostname(serviceName, es.Namespace, c.opts.DomainSuffix)
				svcs := []*model.Service{
					c.GetService(hostname),
				}
				if features.EnableMCSHost {
					svcs = append(svcs, c.GetService(serviceClusterSetLocalHostname(types.NamespacedName{Namespace: es.Namespace, Name: serviceName})))
				}
				for _, svc := range svcs {
					if svc == nil {
						continue
					}

					for _, esPort := range es.Ports {
						port, f := svc.Ports.Get(ptr.OrEmpty(esPort.Name))
						if !f {
							log.Warnf("unexpected state, svc %v missing port %v", svc.Hostname, esPort.Name)
							continue
						}

						// If the endpoint isn't ready, report this
						if endpointHealthStatus(svc, ep) == model.UnHealthy && c.opts.Metrics != nil {
							c.opts.Metrics.AddMetric(model.ProxyStatusEndpointNotReady, proxy.ID, proxy.ID, "")
						}

						si := model.ServiceTarget{
							Service: svc,
							Port: model.ServiceInstancePort{
								ServicePort: port,
								TargetPort:  uint32(ptr.OrEmpty(esPort.Port)),
							},
						}
						out = append(out, si)
					}
				}
			}
		}
		return out
	}

	return nil
}

func (c *KrtController) serviceTargetsByPod(pod *v1.Pod, svc *model.Service) []model.ServiceTarget {
	var out []model.ServiceTarget

	tps := make(map[model.Port]*model.Port)
	tpsList := make([]model.Port, 0)
	for _, svcPort := range svc.Ports {
		if svcPort.Protocol == protocol.UDP {
			continue
		}

		// find target port
		portNum, err := findServicePort(pod, svcPort)
		if err != nil {
			log.Debugf("Failed to find port for service %s/%s: %v", svc.Attributes.Namespace, svc.Attributes.Name, err)
			continue
		}
		// Dedupe the target ports here - Service might have configured multiple ports to the same target port,
		// we will have to create only one ingress listener per port and protocol so that we do not endup
		// complaining about listener conflicts.
		targetPort := model.Port{
			Port:     portNum,
			Protocol: svcPort.Protocol,
		}
		if _, exists := tps[targetPort]; !exists {
			tps[targetPort] = svcPort
			tpsList = append(tpsList, targetPort)
		}
	}
	// Iterate over target ports in the same order as defined in service spec, in case of
	// protocol conflict for a port causes unstable protocol selection for a port.
	for _, tp := range tpsList {
		svcPort := tps[tp]
		out = append(out, model.ServiceTarget{
			Service: svc,
			Port: model.ServiceInstancePort{
				ServicePort: svcPort,
				TargetPort:  uint32(tp.Port),
			},
		})
	}

	return out
}

func findServicePort(pod *v1.Pod, svcPort *model.Port) (int, error) {
	portName := svcPort.TargetPort
	switch portName.Type {
	case intstr.String:
		name := portName.StrVal
		for _, container := range pod.Spec.Containers {
			for _, port := range container.Ports {
				if port.Name == name {
					return int(port.ContainerPort), nil
				}
			}
		}
		// Also search native sidecar init containers (restartPolicy=Always).
		for _, container := range pod.Spec.InitContainers {
			if container.RestartPolicy == nil || *container.RestartPolicy != v1.ContainerRestartPolicyAlways {
				continue
			}
			for _, port := range container.Ports {
				if port.Name == name {
					return int(port.ContainerPort), nil
				}
			}
		}
	case intstr.Int:
		return portName.IntValue(), nil
	}

	return 0, fmt.Errorf("no suitable port for manifest: %s", pod.UID)
}

// getProxyServiceTargetsFromMetadata retrieves ServiceTargets using proxy Metadata rather than
// from the Pod. This allows retrieving Instances immediately, regardless of delays in Kubernetes.
// If the proxy doesn't have enough metadata, an error is returned
func (c *KrtController) getProxyServiceTargetsFromMetadata(proxy *model.Proxy) ([]model.ServiceTarget, error) {
	if len(proxy.Labels) == 0 {
		return nil, nil
	}

	// Find the Service associated with the pod.
	services := c.outputs.JoinedServicesByNamespace.Lookup(proxy.ConfigNamespace)
	slices.FilterInPlace(services, func(s *model.Service) bool {
		return labels.Instance(s.Attributes.LabelSelectors).Match(proxy.Labels)
	})
	if len(services) == 0 {
		return nil, fmt.Errorf("no instances found for %s", proxy.ID)
	}

	out := make([]model.ServiceTarget, 0)
	for _, svc := range services {
		tps := make(map[model.Port]*model.Port)
		tpsList := make([]model.Port, 0)
		for _, port := range svc.Ports {
			var portNum int
			if len(proxy.Metadata.PodPorts) > 0 {
				var err error
				portNum, err = findServicePortFromMetadata(port, proxy.Metadata.PodPorts)
				if err != nil {
					return nil, fmt.Errorf("failed to find target port for %v: %v", proxy.ID, err)
				}
			} else {
				// most likely a VM - we assume the WorkloadEntry won't remap any ports
				portNum = port.TargetPort.IntValue()
			}

			// Dedupe the target ports here - Service might have configured multiple ports to the same target port,
			// we will have to create only one ingress listener per port and protocol so that we do not endup
			// complaining about listener conflicts.
			targetPort := model.Port{
				Port:     portNum,
				Protocol: port.Protocol,
			}
			if _, exists := tps[targetPort]; !exists {
				tps[targetPort] = port
				tpsList = append(tpsList, targetPort)
			}
		}

		// Iterate over target ports in the same order as defined in service spec, in case of
		// protocol conflict for a port causes unstable protocol selection for a port.
		for _, tp := range tpsList {
			svcPort := tps[tp]
			out = append(out, model.ServiceTarget{
				Service: svc,
				Port: model.ServiceInstancePort{
					ServicePort: svcPort,
					TargetPort:  uint32(tp.Port),
				},
			})
		}
	}
	return out, nil
}

// findPortFromMetadata resolves the TargetPort of a Service Port, by reading the Pod spec.
func findServicePortFromMetadata(svcPort *model.Port, podPorts []model.PodPort) (int, error) {
	target := svcPort.TargetPort

	switch target.Type {
	case intstr.String:
		name := target.StrVal
		for _, port := range podPorts {
			if port.Name == name && port.Protocol == string(svcPort.Protocol) {
				return port.ContainerPort, nil
			}
		}
	case intstr.Int:
		// For a direct reference we can just return the port number
		return target.IntValue(), nil
	}

	return 0, fmt.Errorf("no matching port found for %+v", svcPort)
}

func (c *KrtController) GetProxyWorkloadLabels(proxy *model.Proxy) labels.Instance {
	key := podKeyByProxy(proxy)
	var pod *v1.Pod
	if key.Name != "" {
		pod = ptr.Flatten(c.inputs.Pods.GetKey(key.String()))
	} else if c.features.EnableProxyFindPodByIP {
		pods := c.inputs.PodsByIP.Lookup(proxy.IPAddresses[0])
		if len(pods) > 1 {
			// This should only happen with hostNetwork pods, which cannot be proxy clients...
			log.Errorf("unexpected: found multiple pods for proxy %v (%v)", proxy.ID, proxy.IPAddresses[0])
		}
		for _, p := range pods {
			if p.Namespace == proxy.ConfigNamespace {
				pod = p
				break
			}
		}
	}

	if pod != nil {
		locality := podLocality(nil, pod, c.inputs.Nodes)
		nodeName := proxy.GetNodeName()
		return labelutil.AugmentLabels(pod.Labels, c.clusterID, locality, nodeName, c.Network(pod.Status.PodIP, pod.Labels))
	}

	return nil
}

func (c *KrtController) exportedServices() []exportedService {
	exports := c.inputs.ServiceExports.List()
	out := make([]exportedService, 0, len(exports))

	for _, export := range exports {
		es := exportedService{
			namespacedName:  config.NamespacedName(export),
			discoverability: make(map[host.Name]string),
		}

		// Generate the map of all hosts for this service to their discoverability policies.
		clusterLocalHost := kubesr.ServiceHostname(export.GetName(), export.GetNamespace(), c.opts.DomainSuffix)
		clusterSetLocalHost := serviceClusterSetLocalHostname(es.namespacedName)
		for _, hostName := range []host.Name{clusterLocalHost, clusterSetLocalHost} {
			if svc := c.outputs.JoinedServicesByHostname.Lookup(string(hostName)); len(svc) > 0 {
				es.discoverability[hostName] = endpointDiscoverabilityPolicy(nil, c.inputs.ServiceExports, svc[0], c.features).String()
			}
		}

		out = append(out, es)
	}

	return out
}

func (c *KrtController) importedServices() []importedService {
	return slices.Map(c.outputs.MCSServices.List(), func(svc *model.Service) importedService {
		info := importedService{
			namespacedName: svc.NamespacedName(),
		}
		if vips := svc.ClusterVIPs.GetAddressesFor(c.Cluster()); len(vips) > 0 {
			info.clusterSetVIP = vips[0]
		}
		return info
	})
}

// MCSServices returns information about the services that have been exported/imported via the
// Kubernetes Multi-Cluster Services (MCS) ServiceExport API. Only applies to services in
// Kubernetes clusters.
func (c *KrtController) MCSServices() []model.MCSServiceInfo {
	outMap := make(map[types.NamespacedName]model.MCSServiceInfo)

	// Add the ServiceExport info.
	for _, se := range c.exportedServices() {
		mcsService := outMap[se.namespacedName]
		mcsService.Cluster = c.Cluster()
		mcsService.Name = se.namespacedName.Name
		mcsService.Namespace = se.namespacedName.Namespace
		mcsService.Exported = true
		mcsService.Discoverability = se.discoverability
		outMap[se.namespacedName] = mcsService
	}

	// Add the ServiceImport info.
	for _, si := range c.importedServices() {
		mcsService := outMap[si.namespacedName]
		mcsService.Cluster = c.Cluster()
		mcsService.Name = si.namespacedName.Name
		mcsService.Namespace = si.namespacedName.Namespace
		mcsService.Imported = true
		mcsService.ClusterSetVIP = si.clusterSetVIP
		outMap[si.namespacedName] = mcsService
	}

	return maps.Values(outMap)
}

func (c *KrtController) Run(stop <-chan struct{}) {
	if c.opts.SyncTimeout != 0 {
		time.AfterFunc(c.opts.SyncTimeout, func() {
			if !c.HasSynced() {
				log.Warnf("kube controller for %s initial sync timed out", c.opts.ClusterID)
				c.initialSyncTimedout.Store(true)
			}
		})
	}

	<-stop
	close(c.stop)
	log.Infof("Controller terminated")
}

// HasSynced returns true after the initial state synchronization
func (c *KrtController) HasSynced() bool {
	if c.initialSyncTimedout.Load() {
		return true
	}

	if !c.krtNetworkManager.HasSynced() {
		return false
	}

	if !c.inputs.Pods.HasSynced() ||
		!c.inputs.Nodes.HasSynced() ||
		!c.inputs.Namespaces.HasSynced() ||
		!c.inputs.EndpointSlices.HasSynced() ||
		!c.outputs.JoinedServices.HasSynced() ||
		!c.outputs.ServiceEndpoints.HasSynced() {

		return false
	}

	// MCS collections are optional - only check them when enabled.
	if c.inputs.ServiceExports != nil && !c.inputs.ServiceExports.HasSynced() {
		return false
	}
	if c.inputs.ServiceImports != nil && !c.inputs.ServiceImports.HasSynced() {
		return false
	}

	return true
}

func (c *KrtController) Cleanup() error {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	c.closed.Store(true)

	c.xdsUpdater.RemoveShard(model.ShardKeyFromRegistry(c))

	// Unregister networks handler
	if c.networksHandlerRegistration != nil {
		c.opts.MeshNetworksWatcher.DeleteNetworksHandler(c.networksHandlerRegistration)
	}

	return nil
}

// AppendServiceHandler implements a service catalog operation
func (c *KrtController) AppendServiceHandler(f model.ServiceHandler) *model.ServiceHandler {
	return c.handlers.AppendServiceHandler(f)
}

// UnregisterServiceHandler removes a handler previously registered via AppendServiceHandler.
func (c *KrtController) UnregisterServiceHandler(handle *model.ServiceHandler) {
	c.handlers.UnregisterServiceHandler(handle)
}

// AppendWorkloadHandler implements a service catalog operation
func (c *KrtController) AppendWorkloadHandler(f func(*model.WorkloadInstance, model.Event)) {
	c.handlers.AppendWorkloadHandler(f)
}

// WorkloadInstanceHandler defines the handler for service instances generated by other registries
func (c *KrtController) WorkloadInstanceHandler(si *model.WorkloadInstance, event model.Event) {
	if !c.features.EnableK8SServiceSelectWorkloadEntries {
		return
	}

	if si.Namespace == "" || len(si.Endpoint.Labels) == 0 {
		return
	}

	switch event {
	case model.EventDelete:
		c.inputs.WorkloadInstances.DeleteObject(si.ResourceName())
	default:
		c.inputs.WorkloadInstances.ConditionalUpdateObject(si)
	}
}
