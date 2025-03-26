package virtualservice

import (
	"fmt"
	"strings"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/util/protoconv"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/config/schema/collection"
	"istio.io/istio/pkg/config/schema/collections"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/gvr"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/config/visibility"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/kclient"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/kube/kubetypes"
	"istio.io/istio/pkg/log"
	"istio.io/istio/pkg/maps"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/protomarshal"
	"istio.io/istio/pkg/util/sets"
	"k8s.io/apimachinery/pkg/types"

	networking "istio.io/api/networking/v1alpha3"
	networkingclient "istio.io/client-go/pkg/apis/networking/v1"
)

var errUnsupportedOp = fmt.Errorf("unsupported operation: the virtual service config store is a read-only view")

type MeshConfig = meshwatcher.MeshConfigResource

// Controller defines the controller for the gateway-api. The controller reads a variety of resources (Gateway types, as well
// as adjacent types like Namespace and Service), and through `krt`, translates them into Istio types (Gateway/VirtualService).
//
// Most resources are fully "self-contained" with krt, but there are a few usages breaking out of `krt`; these are managed by `krt.RecomputeProtected`.
// These are recomputed on each new PushContext initialization, which will call Controller.Reconcile().
//
// The generated Istio types are not stored in the cluster at all and are purely internal. Calls to List() (from PushContext)
// will expose these. They can be introspected at /debug/configz.
//
// The status on all gateway-api types is also tracked. Each collection emits downstream objects, but also status about the
// input type. If the status changes, it is queued to asynchronously update the status of the object in Kubernetes.
type Controller struct {
	xdsUpdater model.XDSUpdater

	// Handlers tracks all registered handlers, so that syncing can be detected
	handler krt.HandlerRegistration

	// outputs contains all the output collections for this controller.
	// Currently, the only usage of this controller is from non-krt things (PushContext) so this is not exposed directly.
	// If desired in the future, it could be.
	output krt.Collection[*config.Config]

	stop chan struct{}
}

var _ model.ConfigStoreController = &Controller{}

type Options struct {
	Clients []kube.Client
	Client  kube.Client

	XDSUpdater model.XDSUpdater

	KrtDebugger *krt.DebugHandler
	MeshConfig  krt.Singleton[MeshConfig]
}

func New(options Options) *Controller {
	stop := make(chan struct{})
	opts := krt.NewOptionsBuilder(stop, "gateway", options.KrtDebugger)

	c := &Controller{
		xdsUpdater: options.XDSUpdater,
		stop:       stop,
	}

	filter := kclient.Filter{
		ObjectFilter: options.Client.ObjectFilter(),
	}

	MeshConfig := options.MeshConfig

	VSCollections := make([]krt.Collection[*networkingclient.VirtualService], 0, len(options.Clients))
	for _, client := range options.Clients {
		virtualservices := kclient.NewDelayedInformer[*networkingclient.VirtualService](client,
			gvr.VirtualService, kubetypes.StandardInformer, filter)
		VSCollections = append(VSCollections, krt.WrapClient(virtualservices, opts.WithName("informer/VirtualServices")...))
	}

	VirtualServices := krt.JoinCollection(VSCollections, opts.WithName("VirtualServices")...)

	DefaultExportTo := krt.NewSingleton(func(ctx krt.HandlerContext) *sets.Set[visibility.Instance] {
		meshCfg := krt.FetchOne(ctx, MeshConfig.AsCollection())

		exports := sets.New[visibility.Instance]()
		if meshCfg.DefaultVirtualServiceExportTo != nil {
			for _, e := range meshCfg.DefaultVirtualServiceExportTo {
				exports.Insert(visibility.Instance(e))
			}
		} else {
			exports.Insert(visibility.Public)
		}

		return &exports
	}, opts.WithName("DefaultExportTo")...)

	DelegateVirtualServices := krt.NewCollection(VirtualServices, func(ctx krt.HandlerContext, vs *networkingclient.VirtualService) *DelegateVirtualService {
		// this is a Root VS, we won't add these to the collection directly
		if len(vs.Spec.Hosts) > 0 {
			return nil
		}

		var exportToSet sets.Set[visibility.Instance]
		if len(vs.Spec.ExportTo) == 0 {
			// No exportTo in virtualService. Use the global default
			defaultExportTo := *krt.FetchOne(ctx, DefaultExportTo.AsCollection())
			exportToSet = sets.NewWithLength[visibility.Instance](defaultExportTo.Len())
			for v := range defaultExportTo {
				if v == visibility.Private {
					exportToSet.Insert(visibility.Instance(vs.Namespace))
				} else {
					exportToSet.Insert(v)
				}
			}
		} else {
			exportToSet = sets.NewWithLength[visibility.Instance](len(vs.Spec.ExportTo))
			for _, e := range vs.Spec.ExportTo {
				if e == string(visibility.Private) {
					exportToSet.Insert(visibility.Instance(vs.Namespace))
				} else {
					exportToSet.Insert(visibility.Instance(e))
				}
			}
		}

		return &DelegateVirtualService{
			Spec:      &vs.Spec,
			Name:      vs.Name,
			Namespace: vs.Namespace,
			ExportTo:  exportToSet,
		}
	}, opts.WithName("DelegateVirtualServices")...)

	MergedVirtualServices := krt.NewCollection(VirtualServices, func(ctx krt.HandlerContext, vs *networkingclient.VirtualService) **config.Config {
		// this is a Delegate VS, we won't add these to the collection directly
		if len(vs.Spec.Hosts) == 0 {
			return nil
		}

		// we need to deep copy the VS always because resolveVirtualServiceShortnames mutates the VS
		root := vs.DeepCopy()

		if !isRootVs(&root.Spec) {
			config := resolveVirtualServiceShortnames(root)
			return &config
		}

		mergedRoutes := []*networking.HTTPRoute{}
		for _, http := range root.Spec.Http {
			if delegate := http.Delegate; delegate != nil {
				delegateNamespace := delegate.Namespace
				if delegateNamespace == "" {
					delegateNamespace = root.Namespace
				}

				key := types.NamespacedName{Namespace: delegate.Namespace, Name: delegate.Name}
				delegateVs := krt.FetchOne(ctx, DelegateVirtualServices, krt.FilterObjectName(key))
				if delegateVs == nil {
					log.Warnf("delegate virtual service %s/%s of %s/%s not found",
						delegateNamespace, delegate.Name, root.Namespace, root.Name)
					// delegate not found, ignore only the current HTTP route
					continue
				}

				// make sure that the delegate is visible to root virtual service's namespace
				if !delegateVs.ExportTo.Contains(visibility.Public) && !delegateVs.ExportTo.Contains(visibility.Instance(root.Namespace)) {
					log.Warnf("delegate virtual service %s/%s of %s/%s is not exported to %s",
						delegateNamespace, delegate.Name, root.Namespace, root.Name, root.Namespace)
					continue
				}

				// DeepCopy to prevent mutate the original delegate, it can conflict
				// when multiple routes delegate to one single VS.
				copiedDelegate := delegateVs.Spec.DeepCopy()
				merged := mergeHTTPRoutes(http, copiedDelegate.Http)
				mergedRoutes = append(mergedRoutes, merged...)
			} else {
				mergedRoutes = append(mergedRoutes, http)
			}
		}

		root.Spec.Http = mergedRoutes
		if log.DebugEnabled() {
			vsString, _ := protomarshal.ToJSONWithIndent(&root.Spec, "   ")
			log.Debugf("merged virtualService: %s", vsString)
		}

		config := resolveVirtualServiceShortnames(root)
		return &config
	}, opts.WithName("MergedVirtualServices")...)

	handler := MergedVirtualServices.RegisterBatch(func(events []krt.Event[*config.Config]) {
		if c.xdsUpdater == nil {
			return
		}
		cu := sets.New[model.ConfigKey]()
		for _, e := range events {
			vs := e.Latest()
			c := model.ConfigKey{
				Kind:      kind.VirtualService,
				Name:      vs.Name,
				Namespace: vs.Namespace,
			}
			cu.Insert(c)
		}
		if len(cu) == 0 {
			return
		}
		c.xdsUpdater.ConfigUpdate(&model.PushRequest{
			Full:           true,
			ConfigsUpdated: cu,
			Reason:         model.NewReasonStats(model.ConfigUpdate),
		})
	}, false)

	c.handler = handler
	c.output = MergedVirtualServices

	return c
}

func (c *Controller) Schemas() collection.Schemas {
	return collection.SchemasFor(
		collections.VirtualService,
	)
}

func (c *Controller) Get(typ config.GroupVersionKind, name, namespace string) *config.Config {
	return nil
}

func (c *Controller) List(typ config.GroupVersionKind, namespace string) []config.Config {
	if typ != gvk.VirtualService {
		return nil
	}

	return slices.Map(c.output.List(), func(e *config.Config) config.Config {
		return *e
	})
}

func (c *Controller) Create(config config.Config) (revision string, err error) {
	return "", errUnsupportedOp
}

func (c *Controller) Update(config config.Config) (newRevision string, err error) {
	return "", errUnsupportedOp
}

func (c *Controller) UpdateStatus(config config.Config) (newRevision string, err error) {
	return "", errUnsupportedOp
}

func (c *Controller) Patch(orig config.Config, patchFn config.PatchFunc) (string, error) {
	return "", errUnsupportedOp
}

func (c *Controller) Delete(typ config.GroupVersionKind, name, namespace string, _ *string) error {
	return errUnsupportedOp
}

func (c *Controller) RegisterEventHandler(typ config.GroupVersionKind, handler model.EventHandler) {
}

func (c *Controller) Run(stop <-chan struct{}) {
	<-stop
	close(c.stop)
}

func (c *Controller) HasSynced() bool {
	return c.output.HasSynced() && c.handler.HasSynced()
}

type DelegateVirtualService struct {
	Spec      *networking.VirtualService
	Name      string
	Namespace string
	ExportTo  sets.Set[visibility.Instance]
}

func (dvs DelegateVirtualService) ResourceName() string {
	return types.NamespacedName{Namespace: dvs.Namespace, Name: dvs.Name}.String()
}

func (dvs DelegateVirtualService) Equals(other DelegateVirtualService) bool {
	return protoconv.Equals(dvs.Spec, other.Spec)
}

func isRootVs(vs *networking.VirtualService) bool {
	for _, route := range vs.Http {
		// it is root vs with delegate
		if route.Delegate != nil {
			return true
		}
	}
	return false
}

// merge root's route with delegate's and the merged route number equals the delegate's.
// if there is a conflict with root, the route is ignored
func mergeHTTPRoutes(root *networking.HTTPRoute, delegate []*networking.HTTPRoute) []*networking.HTTPRoute {
	root.Delegate = nil

	out := make([]*networking.HTTPRoute, 0, len(delegate))
	for _, subRoute := range delegate {
		merged := mergeHTTPRoute(root, subRoute)
		if merged != nil {
			out = append(out, merged)
		}
	}
	return out
}

// merge the two HTTPRoutes, if there is a conflict with root, the delegate route is ignored
func mergeHTTPRoute(root *networking.HTTPRoute, delegate *networking.HTTPRoute) *networking.HTTPRoute {
	// suppose there are N1 match conditions in root, N2 match conditions in delegate
	// if match condition of N2 is a subset of anyone in N1, this is a valid matching conditions
	merged, conflict := mergeHTTPMatchRequests(root.Match, delegate.Match)
	if conflict {
		log.Warnf("HTTPMatchRequests conflict: root route %s, delegate route %s", root.Name, delegate.Name)
		return nil
	}
	delegate.Match = merged

	if delegate.Name == "" {
		delegate.Name = root.Name
	} else if root.Name != "" {
		delegate.Name = root.Name + "-" + delegate.Name
	}
	if delegate.Rewrite == nil {
		delegate.Rewrite = root.Rewrite
	}
	if delegate.DirectResponse == nil {
		delegate.DirectResponse = root.DirectResponse
	}
	if delegate.Timeout == nil {
		delegate.Timeout = root.Timeout
	}
	if delegate.Retries == nil {
		delegate.Retries = root.Retries
	}
	if delegate.Fault == nil {
		delegate.Fault = root.Fault
	}
	if delegate.Mirror == nil {
		delegate.Mirror = root.Mirror
	}
	// nolint: staticcheck
	if delegate.MirrorPercent == nil {
		delegate.MirrorPercent = root.MirrorPercent
	}
	if delegate.MirrorPercentage == nil {
		delegate.MirrorPercentage = root.MirrorPercentage
	}
	if delegate.CorsPolicy == nil {
		delegate.CorsPolicy = root.CorsPolicy
	}
	if delegate.Mirrors == nil {
		delegate.Mirrors = root.Mirrors
	}
	if delegate.Headers == nil {
		delegate.Headers = root.Headers
	}
	return delegate
}

// return merged match conditions if not conflicts
func mergeHTTPMatchRequests(root, delegate []*networking.HTTPMatchRequest) (out []*networking.HTTPMatchRequest, conflict bool) {
	if len(root) == 0 {
		return delegate, false
	}

	if len(delegate) == 0 {
		return root, false
	}

	// each HTTPMatchRequest of delegate must find a superset in root.
	// otherwise it conflicts
	for _, subMatch := range delegate {
		foundMatch := false
		for _, rootMatch := range root {
			if hasConflict(rootMatch, subMatch) {
				log.Warnf("HTTPMatchRequests conflict: root %v, delegate %v", rootMatch, subMatch)
				continue
			}
			// merge HTTPMatchRequest
			out = append(out, mergeHTTPMatchRequest(rootMatch, subMatch))
			foundMatch = true
		}
		if !foundMatch {
			return nil, true
		}
	}
	if len(out) == 0 {
		conflict = true
	}
	return
}

func mergeHTTPMatchRequest(root, delegate *networking.HTTPMatchRequest) *networking.HTTPMatchRequest {
	// nolint: govet
	out := *delegate
	if out.Name == "" {
		out.Name = root.Name
	} else if root.Name != "" {
		out.Name = root.Name + "-" + out.Name
	}
	if out.Uri == nil {
		out.Uri = root.Uri
	}
	if out.Scheme == nil {
		out.Scheme = root.Scheme
	}
	if out.Method == nil {
		out.Method = root.Method
	}
	if out.Authority == nil {
		out.Authority = root.Authority
	}
	// headers
	out.Headers = maps.MergeCopy(root.Headers, delegate.Headers)

	// withoutheaders
	out.WithoutHeaders = maps.MergeCopy(root.WithoutHeaders, delegate.WithoutHeaders)

	// queryparams
	out.QueryParams = maps.MergeCopy(root.QueryParams, delegate.QueryParams)

	if out.Port == 0 {
		out.Port = root.Port
	}

	// SourceLabels
	out.SourceLabels = maps.MergeCopy(root.SourceLabels, delegate.SourceLabels)

	if out.SourceNamespace == "" {
		out.SourceNamespace = root.SourceNamespace
	}

	if len(out.Gateways) == 0 {
		out.Gateways = root.Gateways
	}

	if len(out.StatPrefix) == 0 {
		out.StatPrefix = root.StatPrefix
	}
	return &out
}

func hasConflict(root, leaf *networking.HTTPMatchRequest) bool {
	roots := []*networking.StringMatch{root.Uri, root.Scheme, root.Method, root.Authority}
	leaves := []*networking.StringMatch{leaf.Uri, leaf.Scheme, leaf.Method, leaf.Authority}
	for i := range roots {
		if stringMatchConflict(roots[i], leaves[i]) {
			return true
		}
	}
	// header conflicts
	for key, leafHeader := range leaf.Headers {
		if stringMatchConflict(root.Headers[key], leafHeader) {
			return true
		}
	}

	// without headers
	for key, leafValue := range leaf.WithoutHeaders {
		if stringMatchConflict(root.WithoutHeaders[key], leafValue) {
			return true
		}
	}

	// query params conflict
	for key, value := range leaf.QueryParams {
		if stringMatchConflict(root.QueryParams[key], value) {
			return true
		}
	}

	if root.IgnoreUriCase != leaf.IgnoreUriCase {
		return true
	}
	if root.Port > 0 && leaf.Port > 0 && root.Port != leaf.Port {
		return true
	}

	// sourceNamespace
	if root.SourceNamespace != "" && leaf.SourceNamespace != root.SourceNamespace {
		return true
	}

	// sourceLabels should not conflict, root should have superset of sourceLabels.
	for key, leafValue := range leaf.SourceLabels {
		if v, ok := root.SourceLabels[key]; ok && v != leafValue {
			return true
		}
	}

	// gateways should not conflict, root should have superset of gateways.
	if len(root.Gateways) > 0 && len(leaf.Gateways) > 0 {
		if len(root.Gateways) < len(leaf.Gateways) {
			return true
		}
		rootGateway := sets.New(root.Gateways...)
		for _, gw := range leaf.Gateways {
			if !rootGateway.Contains(gw) {
				return true
			}
		}
	}

	return false
}

func stringMatchConflict(root, leaf *networking.StringMatch) bool {
	// no conflict when root or leaf is not specified
	if root == nil || leaf == nil {
		return false
	}
	// If root regex match is specified, delegate should not have other matches.
	if root.GetRegex() != "" {
		if leaf.GetRegex() != "" || leaf.GetPrefix() != "" || leaf.GetExact() != "" {
			return true
		}
	}
	// If delegate regex match is specified, root should not have other matches.
	if leaf.GetRegex() != "" {
		if root.GetRegex() != "" || root.GetPrefix() != "" || root.GetExact() != "" {
			return true
		}
	}
	// root is exact match
	if exact := root.GetExact(); exact != "" {
		// leaf is prefix match, conflict
		if leaf.GetPrefix() != "" {
			return true
		}
		// both exact, but not equal
		if leaf.GetExact() != exact {
			return true
		}
		return false
	}
	// root is prefix match
	if prefix := root.GetPrefix(); prefix != "" {
		// leaf is prefix match
		if leaf.GetPrefix() != "" {
			// leaf(`/a`) is not subset of root(`/a/b`)
			return !strings.HasPrefix(leaf.GetPrefix(), prefix)
		}
		// leaf is exact match
		if leaf.GetExact() != "" {
			// leaf(`/a`) is not subset of root(`/a/b`)
			return !strings.HasPrefix(leaf.GetExact(), prefix)
		}
	}

	return true
}

func resolveVirtualServiceShortnames(vs *networkingclient.VirtualService) *config.Config {
	// Kubernetes Gateway API semantics do not support shortnames
	cfg := vsToConfig(vs)
	if UseGatewaySemantics(vs) {
		return cfg
	}

	rule := &vs.Spec
	meta := cfg.Meta

	// resolve top level hosts
	for i, h := range rule.Hosts {
		rule.Hosts[i] = string(model.ResolveShortnameToFQDN(h, meta))
	}
	// resolve gateways to bind to
	for i, g := range rule.Gateways {
		if g != constants.IstioMeshGateway {
			rule.Gateways[i] = resolveGatewayName(g, meta)
		}
	}
	// resolve host in http route.destination, route.mirror
	for _, d := range rule.Http {
		for _, m := range d.Match {
			for i, g := range m.Gateways {
				if g != constants.IstioMeshGateway {
					m.Gateways[i] = resolveGatewayName(g, meta)
				}
			}
		}
		for _, w := range d.Route {
			if w.Destination != nil {
				w.Destination.Host = string(model.ResolveShortnameToFQDN(w.Destination.Host, meta))
			}
		}
		if d.Mirror != nil {
			d.Mirror.Host = string(model.ResolveShortnameToFQDN(d.Mirror.Host, meta))
		}
		for _, m := range d.Mirrors {
			if m.Destination != nil {
				m.Destination.Host = string(model.ResolveShortnameToFQDN(m.Destination.Host, meta))
			}
		}
	}
	// resolve host in tcp route.destination
	for _, d := range rule.Tcp {
		for _, m := range d.Match {
			for i, g := range m.Gateways {
				if g != constants.IstioMeshGateway {
					m.Gateways[i] = resolveGatewayName(g, meta)
				}
			}
		}
		for _, w := range d.Route {
			if w.Destination != nil {
				w.Destination.Host = string(model.ResolveShortnameToFQDN(w.Destination.Host, meta))
			}
		}
	}
	// resolve host in tls route.destination
	for _, tls := range rule.Tls {
		for _, m := range tls.Match {
			for i, g := range m.Gateways {
				if g != constants.IstioMeshGateway {
					m.Gateways[i] = resolveGatewayName(g, meta)
				}
			}
		}
		for _, w := range tls.Route {
			if w.Destination != nil {
				w.Destination.Host = string(model.ResolveShortnameToFQDN(w.Destination.Host, meta))
			}
		}
	}
	return cfg
}

func UseGatewaySemantics(vs *networkingclient.VirtualService) bool {
	return vs.Annotations[constants.InternalRouteSemantics] == constants.RouteSemanticsGateway
}

func vsToConfig(vs *networkingclient.VirtualService) *config.Config {
	return &config.Config{
		Meta: config.Meta{
			GroupVersionKind:  gvk.VirtualService,
			Name:              vs.Name,
			Namespace:         vs.Namespace,
			Labels:            vs.Labels,
			Annotations:       vs.Annotations,
			ResourceVersion:   vs.ResourceVersion,
			CreationTimestamp: vs.CreationTimestamp.Time,
			OwnerReferences:   vs.OwnerReferences,
			UID:               string(vs.UID),
			Generation:        vs.Generation,
		},
		Spec:   &vs.Spec,
		Status: &vs.Status,
	}
}

// resolveGatewayName uses metadata information to resolve a reference
// to shortname of the gateway to FQDN
func resolveGatewayName(gwname string, meta config.Meta) string {
	out := gwname

	// New way of binding to a gateway in remote namespace
	// is ns/name. Old way is either FQDN or short name
	if !strings.Contains(gwname, "/") {
		if !strings.Contains(gwname, ".") {
			// we have a short name. Resolve to a gateway in same namespace
			out = meta.Namespace + "/" + gwname
		} else {
			// parse namespace from FQDN. This is very hacky, but meant for backward compatibility only
			// This is a legacy FQDN format. Transform name.ns.svc.cluster.local -> ns/name
			i := strings.Index(gwname, ".")
			fqdn := strings.Index(gwname[i+1:], ".")
			if fqdn == -1 {
				out = gwname[i+1:] + "/" + gwname[:i]
			} else {
				out = gwname[i+1:i+1+fqdn] + "/" + gwname[:i]
			}
		}
	} else {
		// remove the . from ./gateway and substitute it with the namespace name
		i := strings.Index(gwname, "/")
		if gwname[:i] == "." {
			out = meta.Namespace + "/" + gwname[i+1:]
		}
	}
	return out
}
