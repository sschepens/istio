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

package route

import (
	"reflect"
	"testing"
	"time"

	discovery "github.com/envoyproxy/go-control-plane/envoy/service/discovery/v3"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pkg/config"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/util/sets"
)

func TestClearRDSCacheOnDelegateUpdate(t *testing.T) {
	xdsCache := model.NewXdsCache()
	defer xdsCache.Close()
	// root virtual service
	root := config.Config{
		Meta: config.Meta{Name: "root", Namespace: "default"},
		Spec: &networking.VirtualService{
			Http: []*networking.HTTPRoute{
				{
					Name: "route",
					Delegate: &networking.Delegate{
						Namespace: "default",
						Name:      "delegate",
					},
				},
			},
		},
	}
	// delegate virtual service
	delegate := model.ConfigKey{Kind: kind.VirtualService, Name: "delegate", Namespace: "default"}
	// rds cache entry
	entry := Cache{
		VirtualServices:         []config.Config{root},
		DelegateVirtualServices: []model.ConfigHash{delegate.HashCode()},
		ListenerPort:            8080,
	}
	resource := &discovery.Resource{Name: "bar"}

	// add resource to cache
	xdsCache.Add(&entry, &model.PushRequest{Start: time.Now()}, resource)
	if got := xdsCache.Get(&entry); got == nil || !reflect.DeepEqual(got, resource) {
		t.Fatal("rds cache was not updated")
	}

	// clear cache when delegate virtual service is updated
	// this func is called by `dropCacheForRequest` in `initPushContext`
	xdsCache.Clear(sets.New(delegate))
	if got := xdsCache.Get(&entry); got != nil {
		t.Fatal("rds cache was not cleared")
	}

	// add resource to cache
	xdsCache.Add(&entry, &model.PushRequest{Start: time.Now()}, resource)
	irrelevantDelegate := model.ConfigKey{Kind: kind.VirtualService, Name: "foo", Namespace: "default"}

	// don't clear cache when irrelevant delegate virtual service is updated
	xdsCache.Clear(sets.New(irrelevantDelegate))
	if got := xdsCache.Get(&entry); got == nil || !reflect.DeepEqual(got, resource) {
		t.Fatal("rds cache was cleared by irrelevant delegate virtual service update")
	}
}

func TestDependentConfigs(t *testing.T) {
	tests := []struct {
		name string
		r    Cache
		want []model.ConfigHash
	}{
		{
			name: "no internal vs",
			r: Cache{
				VirtualServices: []config.Config{
					{
						Meta: config.Meta{
							Name:      "foo",
							Namespace: "default",
						},
					},
				},
			},
			want: []model.ConfigHash{
				model.ConfigKey{
					Kind:      kind.VirtualService,
					Name:      "foo",
					Namespace: "default",
				}.HashCode(),
			},
		},
		{
			name: "single parent",
			r: Cache{
				VirtualServices: []config.Config{
					{
						Meta: config.Meta{
							Name:      "foo-0-istio-autogenerated-k8s-gateway",
							Namespace: "default",
							Annotations: map[string]string{
								constants.InternalRouteSemantics: constants.RouteSemanticsGateway,
								constants.InternalParentNames:    "HTTPRoute/foo.default",
							},
						},
					},
				},
			},
			want: []model.ConfigHash{
				model.ConfigKey{
					Kind:      kind.HTTPRoute,
					Name:      "foo",
					Namespace: "default",
				}.HashCode(),
			},
		},
		{
			name: "multiple parents",
			r: Cache{
				VirtualServices: []config.Config{
					{
						Meta: config.Meta{
							Name:      "foo-1-istio-autogenerated-k8s-gateway",
							Namespace: "default",
							Annotations: map[string]string{
								constants.InternalRouteSemantics: constants.RouteSemanticsGateway,
								constants.InternalParentNames:    "HTTPRoute/foo.default,HTTPRoute/bar.default",
							},
						},
					},
				},
			},
			want: []model.ConfigHash{
				model.ConfigKey{
					Kind:      kind.HTTPRoute,
					Name:      "foo",
					Namespace: "default",
				}.HashCode(),
				model.ConfigKey{
					Kind:      kind.HTTPRoute,
					Name:      "bar",
					Namespace: "default",
				}.HashCode(),
			},
		},
		{
			name: "mixed",
			r: Cache{
				VirtualServices: []config.Config{
					{
						Meta: config.Meta{
							Name:      "foo",
							Namespace: "default",
						},
					},
					{
						Meta: config.Meta{
							Name:      "bar-0-istio-autogenerated-k8s-gateway",
							Namespace: "default",
							Annotations: map[string]string{
								constants.InternalRouteSemantics: constants.RouteSemanticsGateway,
								constants.InternalParentNames:    "HTTPRoute/bar.default,HTTPRoute/baz.default",
							},
						},
					},
				},
			},
			want: []model.ConfigHash{
				model.ConfigKey{
					Kind:      kind.VirtualService,
					Name:      "foo",
					Namespace: "default",
				}.HashCode(),
				model.ConfigKey{
					Kind:      kind.HTTPRoute,
					Name:      "bar",
					Namespace: "default",
				}.HashCode(),
				model.ConfigKey{
					Kind:      kind.HTTPRoute,
					Name:      "baz",
					Namespace: "default",
				}.HashCode(),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.DependentConfigs(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("DependentConfigs got %v, want %v", got, tt.want)
			}
		})
	}
}
