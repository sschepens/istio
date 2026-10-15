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
	"fmt"
	"testing"

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	"istio.io/istio/pkg/test"
	"istio.io/istio/pkg/test/util/assert"
	"istio.io/istio/pkg/util/sets"
)

// This source deliberately has no mutation API.
type staticEndpointIndex map[string]map[string]ServiceEndpoints

func (s staticEndpointIndex) EndpointsForService(serviceName, namespace string) (ServiceEndpoints, bool) {
	shards, found := s[serviceName][namespace]
	return shards, found
}

func (s staticEndpointIndex) Shardz() map[string]map[string]*EndpointShards {
	out := make(map[string]map[string]*EndpointShards)
	for service, namespaces := range s {
		out[service] = make(map[string]*EndpointShards)
		for namespace, shards := range namespaces {
			out[service][namespace] = snapshotServiceEndpoints(shards).DeepCopy()
		}
	}
	return out
}

func TestAggregateEndpointIndex(t *testing.T) {
	test.SetForTest(t, &features.EnableUnsafeAssertions, true)
	kubeShard := ShardKey{Cluster: "cluster1", Provider: provider.Kubernetes}
	externalShard := ShardKey{Cluster: "cluster1", Provider: provider.External}
	mutable := NewMutableEndpointIndex(DisabledCache{})
	mutable.UpdateServiceEndpoints(kubeShard, "svc", "ns", []*IstioEndpoint{
		{Addresses: []string{"10.0.0.1"}, ServicePortName: "http", ServiceAccount: "shared"},
	}, false)
	external := staticEndpointIndex{
		"svc": {
			"ns": SingleShardEndpoints{
				Key:       externalShard,
				Endpoints: []*IstioEndpoint{{Addresses: []string{"10.0.0.2"}, ServicePortName: "http", ServiceAccount: "external"}},
				Accounts:  sets.New("shared", "external"),
			},
			"other": SingleShardEndpoints{Key: externalShard},
		},
	}
	index := NewAggregateEndpointIndex(mutable, external)
	shards, found := index.EndpointsForService("svc", "ns")
	assert.Equal(t, found, true)
	assert.Equal(t, snapshotServiceEndpoints(shards).keys(), []ShardKey{externalShard, kubeShard})
	assert.Equal(t, shards.ServiceAccounts(), sets.New("shared", "external"))
	assert.Equal(t, len(CopyEndpoints(shards, map[string]int{"http": 80}, sets.New(80))[80]), 2)
	assert.Equal(t, index.Shardz()["svc"]["ns"].shards, snapshotServiceEndpoints(shards).shards)
	assert.Equal(t, index.Shardz()["svc"]["ns"].serviceAccounts, shards.ServiceAccounts())

	// A service with no endpoints is distinct from a missing service or namespace.
	_, found = index.EndpointsForService("svc", "other")
	assert.Equal(t, found, true)
	for _, key := range [][2]string{{"svc", "missing"}, {"missing", "ns"}} {
		missing, ok := index.EndpointsForService(key[0], key[1])
		assert.Equal(t, ok, false)
		assert.Equal(t, missing, nil)
	}

	// Debug output owns its endpoint objects, slices, maps, and account sets.
	dump := index.Shardz()
	dump["svc"]["ns"].shards[kubeShard][0].Addresses[0] = "changed"
	dump["svc"]["ns"].shards[externalShard][0].Addresses[0] = "changed"
	dump["svc"]["ns"].serviceAccounts.Delete("external")
	delete(dump["svc"], "other")
	fresh := index.Shardz()
	assert.Equal(t, fresh["svc"]["ns"].shards[kubeShard][0].Addresses[0], "10.0.0.1")
	assert.Equal(t, fresh["svc"]["ns"].shards[externalShard][0].Addresses[0], "10.0.0.2")
	assert.Equal(t, fresh["svc"]["ns"].serviceAccounts, sets.New("shared", "external"))
	assert.Equal(t, len(fresh["svc"]), 2)

	// Removing the mutable contribution must leave the other source visible.
	mutable.DeleteServiceShard(kubeShard, "svc", "ns", false)
	remaining, found := index.EndpointsForService("svc", "ns")
	assert.Equal(t, found, true)
	assert.Equal(t, snapshotServiceEndpoints(remaining).keys(), []ShardKey{externalShard})
	// The existing wrapper delegates to the mutable source and sees the deletion.
	assert.Equal(t, snapshotServiceEndpoints(shards).keys(), []ShardKey{externalShard})
}

func TestAggregateEndpointIndexDisjointShards(t *testing.T) {
	key := ShardKey{Provider: provider.External, Cluster: "cluster1"}
	for _, tt := range []struct {
		name       string
		other      ShardKey
		assertions bool
		duplicate  bool
	}{
		{"duplicate", key, true, true},
		{"different provider", ShardKey{Provider: provider.Kubernetes, Cluster: key.Cluster}, true, false},
		{"different cluster", ShardKey{Provider: key.Provider, Cluster: "cluster2"}, true, false},
		{"assertions disabled", key, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			test.SetForTest(t, &features.EnableUnsafeAssertions, tt.assertions)
			// Even empty endpoint slices claim shard ownership.
			index := NewAggregateEndpointIndex(
				staticEndpointIndex{"svc": {"ns": SingleShardEndpoints{Key: key}}},
				staticEndpointIndex{"svc": {"ns": SingleShardEndpoints{Key: tt.other}}},
			)
			for name, read := range map[string]func(){
				"iteration": func() {
					view, found := index.EndpointsForService("svc", "ns")
					assert.Equal(t, found, true)
					view.ForEachShard(func(ShardKey, []*IstioEndpoint) bool { return true })
				},
				"debug": func() { index.Shardz() },
			} {
				t.Run(name, func(t *testing.T) {
					var got any
					func() {
						defer func() { got = recover() }()
						read()
					}()
					if tt.duplicate {
						assert.Equal(t, got, any(fmt.Sprintf("duplicate endpoint shard %s across aggregate sources", key)))
					} else {
						assert.Equal(t, got, nil)
					}
				})
			}
		})
	}
}

func TestAggregateEndpointIndexLiveView(t *testing.T) {
	key := ShardKey{Cluster: "cluster1"}
	mutable := NewMutableEndpointIndex(DisabledCache{})
	mutable.UpdateServiceEndpoints(key, "svc", "ns", []*IstioEndpoint{
		{Addresses: []string{"10.0.0.1"}, ServiceAccount: "old"},
	}, false)
	static := staticEndpointIndex{"svc": {"ns": SingleShardEndpoints{
		Key: ShardKey{Cluster: "cluster2"}, Accounts: sets.New("static"),
	}}}
	index := NewAggregateEndpointIndex(mutable, static)
	view, found := index.EndpointsForService("svc", "ns")
	assert.Equal(t, found, true)

	// Updates after lookup must be observed when the wrapper is read, including
	// tracked accounts. Lookup does not take a copy of either set of data.
	mutable.UpdateServiceEndpoints(key, "svc", "ns", []*IstioEndpoint{
		{Addresses: []string{"10.0.0.2"}, ServiceAccount: "new"},
	}, false)
	assert.Equal(t, snapshotServiceEndpoints(view).shards[key][0].Addresses, []string{"10.0.0.2"})
	assert.Equal(t, view.ServiceAccounts(), sets.New("new", "static"))
}

func TestAggregateEndpointIndexEmptyAndSingleSource(t *testing.T) {
	empty := NewAggregateEndpointIndex()
	shards, found := empty.EndpointsForService("svc", "ns")
	assert.Equal(t, found, false)
	assert.Equal(t, shards, nil)
	assert.Equal(t, len(empty.Shardz()), 0)

	env := NewEnvironment()
	key := ShardKey{Cluster: "cluster1"}
	env.MutableEndpointIndex.UpdateServiceEndpoints(key, "svc", "ns", []*IstioEndpoint{
		{Addresses: []string{"10.0.0.1"}},
	}, false)
	shards, found = env.EndpointIndex.EndpointsForService("svc", "ns")
	assert.Equal(t, found, true)
	assert.Equal(t, snapshotServiceEndpoints(shards).shards[key][0].Addresses, []string{"10.0.0.1"})
	env.MutableEndpointIndex.DeleteShard(key)
	_, found = env.EndpointIndex.EndpointsForService("svc", "ns")
	assert.Equal(t, found, false)
}

// snapshotServiceEndpoints adapts any read implementation for assertions and debug fixtures.
func snapshotServiceEndpoints(endpoints ServiceEndpoints) *EndpointShards {
	out := &EndpointShards{shards: make(map[ShardKey][]*IstioEndpoint), serviceAccounts: endpoints.ServiceAccounts().Copy()}
	endpoints.ForEachShard(func(key ShardKey, eps []*IstioEndpoint) bool {
		out.shards[key] = append(out.shards[key], eps...)
		return true
	})
	return out
}
