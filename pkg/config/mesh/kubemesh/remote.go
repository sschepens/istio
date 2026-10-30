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

package kubemesh

import (
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/informerfactory"
	"istio.io/istio/pkg/kube/krt"
)

// NewRemoteMeshConfig watches a remote cluster's mesh ConfigMap without making access to it
// a prerequisite for cluster readiness. Until the first successful read, it follows fallback.
// A successful read with no ConfigMap uses mesh defaults, as NewConfigMapSource does. After
// synchronization, informer watch failures preserve the last observed remote configuration.
// All informers, collections and handlers are scoped to opts.Stop(), the cluster generation.
func NewRemoteMeshConfig(
	client kube.Client,
	namespace, name string,
	fallback krt.Singleton[meshwatcher.MeshConfigResource],
	opts krt.OptionsBuilder,
) krt.Singleton[meshwatcher.MeshConfigResource] {
	// Starting the watch separately is not enough: the shared client's RunAndWait also waits
	// for informers that have already been started. Give this optional watch its own factory.
	factory := informerfactory.NewSharedInformerFactory()
	isolated := meshConfigClient{Client: client, factory: factory}
	source := NewConfigMapSource(isolated, namespace, name, MeshConfigKey, opts)
	remote := meshwatcher.NewCollection(opts, source)

	// Fetching remote directly would wait for its initial sync, preventing fallback from
	// becoming available. Bridge its events into an initially synced optional input instead.
	observed := krt.NewStatic[meshwatcher.MeshConfigResource](nil, true, opts.WithName("ObservedRemoteMeshConfig")...)
	registration := remote.AsCollection().Register(func(e krt.Event[meshwatcher.MeshConfigResource]) {
		observed.Set(e.New)
	})
	go func() {
		<-opts.Stop()
		registration.UnregisterHandler()
		factory.Shutdown()
	}()

	return krt.NewSingleton(func(ctx krt.HandlerContext) *meshwatcher.MeshConfigResource {
		if cfg := krt.FetchOne(ctx, observed.AsCollection()); cfg != nil {
			return cfg
		}
		return krt.FetchOne(ctx, fallback.AsCollection())
	}, opts.WithName("EffectiveMeshConfig")...)
}

// meshConfigClient shares API clients and credentials while isolating the optional informer.
type meshConfigClient struct {
	kube.Client
	factory informerfactory.InformerFactory
}

func (c meshConfigClient) Informers() informerfactory.InformerFactory {
	return c.factory
}
