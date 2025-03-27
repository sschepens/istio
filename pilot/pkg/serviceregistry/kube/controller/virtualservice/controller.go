package virtualservice

import (
	"fmt"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config"
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
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/sets"

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

	DelegateVirtualServices := DelegateVirtualServices(
		VirtualServices,
		DefaultExportTo.AsCollection(),
		opts,
	)

	MergedVirtualServices := MergeVirtualServices(
		VirtualServices,
		DelegateVirtualServices,
		opts,
	)

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
