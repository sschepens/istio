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

package multicluster

import (
	"fmt"
	"testing"
	"time"

	"go.uber.org/atomic"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/mesh/kubemesh"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/retry"
)

func TestRemoteMeshConfigDoesNotBlockClusterSync(t *testing.T) {
	for _, mode := range []string{"missing", "present", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			test.SetForTest(t, &features.RemoteClusterTimeout, time.Duration(0))
			cfg := mesh.DefaultMeshConfig()
			cfg.TrustDomain = "local.example"
			local := meshwatcher.NewTestWatcher(cfg)
			var objects []runtime.Object
			if mode != "missing" {
				objects = append(objects, &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "istio", Namespace: "istio-system"}, Data: map[string]string{"mesh": "trustDomain: remote.example"}})
			}
			client := kube.NewFakeClient(objects...)
			denied := atomic.NewBool(mode == "forbidden")
			attempts := atomic.NewInt32(0)
			client.Kube().(*fake.Clientset).PrependReactor("list", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
				attempts.Inc()
				if denied.Load() {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "istio", fmt.Errorf("configmap access denied"))
				}
				return false, nil, nil
			})
			stop := make(chan struct{})
			c := &Cluster{
				ID:                       "remote",
				Client:                   client,
				stop:                     stop,
				initialSync:              atomic.NewBool(false),
				initialSyncTimeout:       atomic.NewBool(false),
				remoteClusterCollections: atomic.NewPointer[remoteClusterCollections](nil),
				SyncedCh:                 make(chan struct{}),
			}
			effective := kubemesh.NewRemoteMeshConfig(client, "istio-system", "istio", local,
				krt.NewOptionsBuilder(stop, "", nil))
			done := make(chan struct{})
			go func() { defer close(done); c.Run(local, nil, Add, nil, nil) }()
			t.Cleanup(func() { close(stop); <-done; client.Shutdown() })
			retry.UntilOrFail(t, func() bool {
				return attempts.Load() > 0 && c.remoteClusterCollections.Load() != nil && c.pods().HasSynced()
			}, retry.Timeout(5*time.Second))
			// Readiness must be real initial synchronization, not the timeout escape hatch.
			retry.UntilOrFail(t, c.initialSync.Load, retry.Timeout(5*time.Second))
			if c.initialSyncTimeout.Load() {
				t.Fatal("cluster timed out")
			}
			if mode == "forbidden" {
				retry.UntilOrFail(t, func() bool {
					cfg := effective.Get()
					return cfg != nil && cfg.GetTrustDomain() == "local.example"
				})
				updated := mesh.DefaultMeshConfig()
				updated.TrustDomain = "updated-local.example"
				local.Set(updated)
				retry.UntilOrFail(t, func() bool {
					return effective.Get().GetTrustDomain() == "updated-local.example"
				})
				denied.Store(false)
			}
			want := "remote.example"
			if mode == "missing" {
				want = "cluster.local"
			}
			retry.UntilOrFail(t, func() bool {
				cfg := effective.Get()
				return cfg != nil && cfg.GetTrustDomain() == want
			}, retry.Timeout(10*time.Second))
		})
	}
}

// A replacement cluster must become usable even if its credentials cannot read mesh config.
func TestRemoteMeshConfigForbiddenDuringRotation(t *testing.T) {
	test.SetForTest(t, &features.RemoteClusterTimeout, time.Duration(0))
	stop := test.NewStop(t)
	c := buildTestController(t, true)
	c.controller.ClientBuilder = func(_ []byte, _ cluster.ID, _ ...func(*rest.Config)) (kube.Client, error) {
		client := fakeClientWithService("remote-service")
		client.Kube().(*fake.Clientset).PrependReactor("list", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "configmaps"}, "istio", fmt.Errorf("access denied"))
		})
		return client, nil
	}
	// Remote mesh config is constructed by registry components on upstream.
	BuildMultiClusterComponent(c.controller, func(cl *Cluster) testHandler {
		kubemesh.NewRemoteMeshConfig(cl.Client, "istio-system", "istio", meshwatcher.NewTestWatcher(nil),
			krt.NewOptionsBuilder(cl.GetStop(), "", nil))
		return testHandler{ID: cl.ID, Closed: atomic.NewBool(false), Synced: atomic.NewBool(true)}
	})
	c.AddSecret("s0", "c0")
	c.Run(stop)
	retry.UntilOrFail(t, func() bool {
		cl := c.controller.cs.GetByID("c0")
		return cl != nil && clusterReady(cl)
	})
	old := c.controller.cs.GetByID("c0")
	c.AddSecret("s0", "c0")
	retry.UntilOrFail(t, func() bool {
		cl := c.controller.cs.GetByID("c0")
		return cl != nil && cl != old && clusterReady(cl)
	})
	retry.UntilOrFail(t, func() bool {
		select {
		case <-old.GetStop():
			return true
		default:
			return false
		}
	})
	c.DeleteSecret("s0")
	retry.UntilOrFail(t, func() bool {
		return c.controller.cs.GetByID("c0") == nil
	})
}
