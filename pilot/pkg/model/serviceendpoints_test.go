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

package model

import (
	"testing"

	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/util/sets"
)

func TestServiceEndpoints(t *testing.T) {
	externalKey := ShardKey{Provider: provider.External, Cluster: "c2"}
	kubeKey1 := ShardKey{Provider: provider.Kubernetes, Cluster: "c1"}
	kubeKey2 := ShardKey{Provider: provider.Kubernetes, Cluster: "c2"}
	single := SingleShardEndpoints{
		Key:       externalKey,
		Endpoints: []*IstioEndpoint{{Addresses: []string{"10.0.0.1"}, ServicePortName: "http"}},
		// Account tracking is independent of endpoint contents.
		Accounts: sets.New("tracked"),
	}
	mutable := NewMutableEndpointIndex(DisabledCache{})
	for _, key := range []ShardKey{kubeKey2, kubeKey1} {
		mutable.UpdateServiceEndpoints(key, "svc", "ns", []*IstioEndpoint{
			{Addresses: []string{"10.0.0.2"}, ServicePortName: "http", ServiceAccount: "sa"},
		}, false)
	}
	mutableView, _ := mutable.EndpointsForService("svc", "ns")
	static := staticEndpointIndex{"svc": {"ns": single}}
	// Reverse provider order to ensure aggregation orders across sources, rather
	// than simply visiting each source's already ordered contributions.
	aggregate := NewAggregateEndpointIndex(mutable, static)
	aggregateView, _ := aggregate.EndpointsForService("svc", "ns")

	for _, tt := range []struct {
		name     string
		view     ServiceEndpoints
		keys     []ShardKey
		accounts sets.String
	}{
		{"single", single, []ShardKey{externalKey}, sets.New("tracked")},
		{"mutable", mutableView, []ShardKey{kubeKey1, kubeKey2}, sets.New("sa")},
		{"aggregate", aggregateView, []ShardKey{externalKey, kubeKey1, kubeKey2}, sets.New("tracked", "sa")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var keys []ShardKey
			tt.view.ForEachShard(func(key ShardKey, eps []*IstioEndpoint) bool {
				keys = append(keys, key)
				assert.Equal(t, len(eps), 1)
				return true
			})
			assert.Equal(t, keys, tt.keys)

			calls := 0
			tt.view.ForEachShard(func(key ShardKey, _ []*IstioEndpoint) bool {
				calls++
				assert.Equal(t, key, tt.keys[0])
				return false
			})
			assert.Equal(t, calls, 1)
			accounts := tt.view.CopyServiceAccounts()
			assert.Equal(t, accounts, tt.accounts)
			clear(accounts)
			assert.Equal(t, tt.view.CopyServiceAccounts(), tt.accounts)
			assert.Equal(t, len(CopyEndpoints(tt.view, map[string]int{"http": 80}, sets.New(80))[80]), len(tt.keys))
		})
	}
}

func TestCopyEndpoints(t *testing.T) {
	byName := &IstioEndpoint{ServicePortName: "http", EndpointPort: 8080}
	byLegacyPort := &IstioEndpoint{ServicePortName: "http", LegacyClusterPortKey: 81, EndpointPort: 8081}
	single := SingleShardEndpoints{Endpoints: []*IstioEndpoint{
		byName,
		byLegacyPort,
		{ServicePortName: "http", LegacyClusterPortKey: 82}, // legacy port takes precedence, even when excluded
		{ServicePortName: "unknown"},
	}}
	assert.Equal(t, CopyEndpoints(single, map[string]int{"http": 80}, sets.New(80, 81)), map[int][]*IstioEndpoint{
		80: {byName},
		81: {byLegacyPort},
	})
}
