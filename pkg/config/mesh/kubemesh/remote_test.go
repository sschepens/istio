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
	"context"
	"fmt"
	"testing"
	"time"

	"go.uber.org/atomic"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"istio.io/istio/pkg/config/mesh"
	"istio.io/istio/pkg/config/mesh/meshwatcher"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/krt/krttest"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/test/util/retry"
)

func TestRemoteMeshConfigUpdates(t *testing.T) {
	for _, initial := range []string{"", "trustDomain: remote.example", "connectTimeout: -1s"} {
		t.Run(initial, func(t *testing.T) {
			client := kube.NewFakeClient(makeConfigMap("1", map[string]string{key: initial}))
			opts := krttest.Options(t)
			local := mesh.DefaultMeshConfig()
			local.TrustDomain = "local.example"
			fallback := meshwatcher.NewTestWatcher(local)
			effective := NewRemoteMeshConfig(client, namespace, name, fallback, opts)
			client.RunAndWait(opts.Stop())
			waitForDomain := func(want string) {
				t.Helper()
				retry.UntilOrFail(t, func() bool {
					cfg := effective.Get()
					return cfg != nil && cfg.GetTrustDomain() == want
				})
			}
			want := "cluster.local"
			if initial == "trustDomain: remote.example" {
				want = "remote.example"
			}
			waitForDomain(want)
			cms := client.Kube().CoreV1().ConfigMaps(namespace)
			_, err := cms.Update(context.Background(), makeConfigMap("2", map[string]string{
				key: "trustDomain: updated-remote.example",
			}), metav1.UpdateOptions{})
			assert.NoError(t, err)
			waitForDomain("updated-remote.example")

			// Deletion must return to defaults, not the local fallback.
			assert.NoError(t, cms.Delete(context.Background(), name, metav1.DeleteOptions{}))
			waitForDomain("cluster.local")
		})
	}
}

func TestRemoteMeshConfigRetainedOnWatchFailure(t *testing.T) {
	client := kube.NewFakeClient(makeConfigMap("1", map[string]string{key: "trustDomain: remote.example"}))
	stream := watch.NewRaceFreeFake()
	denied := atomic.NewBool(false)
	attempts := atomic.NewInt32(0)
	client.Kube().(*fake.Clientset).PrependWatchReactor("configmaps", func(ktesting.Action) (bool, watch.Interface, error) {
		if denied.Load() {
			attempts.Inc()
			return true, nil, fmt.Errorf("watch unavailable")
		}
		return true, stream, nil
	})
	opts := krttest.Options(t)
	fallback := meshwatcher.NewTestWatcher(nil)
	effective := NewRemoteMeshConfig(client, namespace, name, fallback, opts)
	// The custom watch bypasses the fake client's watch-registration accounting.
	// Start its controllers without waiting on that test-only accounting.
	go client.RunAndWait(opts.Stop())
	retry.UntilOrFail(t, func() bool {
		cfg := effective.Get()
		return cfg != nil && cfg.GetTrustDomain() == "remote.example"
	})
	denied.Store(true)
	stream.Stop()
	retry.UntilOrFail(t, func() bool { return attempts.Load() > 0 }, retry.Timeout(10*time.Second))
	assert.Equal(t, effective.Get().GetTrustDomain(), "remote.example")
}
