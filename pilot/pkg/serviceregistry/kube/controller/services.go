package controller

import (
	"sort"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/aggregate"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/host"
	"istio.io/istio/pkg/config/visibility"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/ptr"
	netutil "istio.io/istio/pkg/util/net"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func Services(
	services krt.Collection[*corev1.Service],
	nodes krt.Collection[*corev1.Node],
	namespaces krt.Collection[*corev1.Namespace],
	domainSuffix string,
	clusterID cluster.ID,
	trustDomain func(krt.HandlerContext) string,
	opts krt.OptionsBuilder,
) krt.Collection[*model.Service] {
	return krt.NewCollection(services, func(ctx krt.HandlerContext, svc *corev1.Service) **model.Service {
		// Get namespace annotations for traffic distribution inheritance
		var nsAnnotations map[string]string
		if ns := krt.FetchOne(ctx, namespaces, krt.FilterKey(svc.Namespace)); ns != nil {
			nsAnnotations = ptr.Flatten(ns).Annotations
		}

		// Create the standard (cluster.local) service.
		svcConv := kube.ConvertService(*svc, nsAnnotations, domainSuffix, clusterID, trustDomain(ctx))

		// skip services that are not discoverable
		if svcConv.Attributes.ExportTo.Contains(visibility.None) {
			return nil
		}

		if isNodePortGatewayService(svc) {
			nodeSelector := getNodeSelectorsForService(svc)
			nodes := krt.Fetch(ctx, nodes, krt.FilterLabel(nodeSelector))

			var nodeAddresses []string
			for _, node := range nodes {
				for _, address := range node.Status.Addresses {
					if address.Type == v1.NodeExternalIP && address.Address != "" {
						nodeAddresses = append(nodeAddresses, address.Address)
						break
					}
				}
			}
			svcConv.Attributes.ClusterExternalAddresses.SetAddressesFor(clusterID, nodeAddresses)
		}

		return &svcConv
	}, opts.WithName("Services")...)
}

type aggregateServiceExternalSource struct {
	meshWideController *aggregate.Controller
}

func (a aggregateServiceExternalSource) List() []*model.Service {
	return a.meshWideController.Services()
}

func (a aggregateServiceExternalSource) GetKey(k string) **model.Service {
	return ptr.Of(a.meshWideController.GetService(host.Name(k)))
}

func (a aggregateServiceExternalSource) Register(h func(krt.Event[*model.Service])) func() {
	handle := a.meshWideController.AppendServiceHandler(func(old, new *model.Service, event model.Event) {
		ev := krt.Event[*model.Service]{
			Event: controllers.EventType(event),
		}
		switch event {
		case model.EventAdd:
			ev.New = &new
		case model.EventDelete:
			ev.Old = &old
		case model.EventUpdate:
			ev.Old = &old
			ev.New = &new
		}
		h(ev)
	})

	return func() {
		a.meshWideController.UnregisterServiceHandler(handle)
	}
}

func (a aggregateServiceExternalSource) HasSynced() bool {
	return true
}

func (a aggregateServiceExternalSource) WaitUntilSynced(stop <-chan struct{}) bool {
	return true
}

func MCSServices(
	serviceImports krt.Collection[controllers.Object],
	meshWideController *aggregate.Controller,
	domainSuffix string,
	clusterID cluster.ID,
	opts krt.OptionsBuilder,
) krt.Collection[*model.Service] {
	externalSource := aggregateServiceExternalSource{meshWideController: meshWideController}
	externalGlobalServices := krt.WrapExternalSource(externalSource, opts.WithName("MeshWideServices")...)
	return krt.NewCollection(serviceImports, func(ctx krt.HandlerContext, obj controllers.Object) **model.Service {
		si := controllers.Extract[*unstructured.Unstructured](obj)
		if si == nil {
			return nil
		}

		realHost := kube.ServiceHostnameForKR(si, domainSuffix)
		realService := ptr.Flatten(krt.FetchOne(ctx, externalGlobalServices, krt.FilterKey(realHost.String())))
		if realService == nil {
			log.Warnf("failed processing event for ServiceImport %s/%s in cluster %s. No matching service found in cluster",
				si.GetNamespace(), si.GetName(), clusterID)
			return nil
		}

		vips := serviceImportIPs(si)

		if len(vips) == 0 {
			return nil
		}

		mcsService := realService.ShallowCopy()
		mcsService.Hostname = serviceClusterSetLocalHostnameForKR(si)
		mcsService.DefaultAddress = vips[0]
		mcsService.ClusterVIPs.SetAddressesFor(clusterID, vips)

		return &mcsService
	}, opts.WithName("MCSServices")...)
}

// serviceImportIPs returns the list of ClusterSet IPs for the ServiceImport.
func serviceImportIPs(si *unstructured.Unstructured) []string {
	var ips []string
	if spec, ok := si.Object["spec"].(map[string]any); ok {
		if rawIPs, ok := spec["ips"].([]any); ok {
			for _, rawIP := range rawIPs {
				ip := rawIP.(string)
				if netutil.IsValidIPAddress(ip) {
					ips = append(ips, ip)
				}
			}
		}
	}
	sort.Strings(ips)
	return ips
}

func (c *KrtController) pushServices() {
	shard := model.ShardKeyFromRegistry(c)
	c.outputs.JoinedServices.RegisterBatch(func(events []krt.Event[*model.Service]) {
		for _, e := range events {
			svc := e.Latest()

			c.xdsUpdater.SvcUpdate(shard, string(svc.Hostname), svc.Attributes.Namespace, model.Event(e.Event))
			if e.Event != controllers.EventDelete {
				log.Debugf("Service %s in namespace %s updated and needs push", svc.Hostname, svc.Attributes.Namespace)
			}
			c.handlers.NotifyServiceHandlers(ptr.Flatten(e.Old), svc, model.Event(e.Event))
		}
	}, false)
}
