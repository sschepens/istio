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

// This source deliberately has no mutation API.
type staticEndpointIndex map[string]map[string]*EndpointShards

func (s staticEndpointIndex) ShardsForService(serviceName, namespace string) (*EndpointShards, bool) {
	shards, found := s[serviceName][namespace]
	return shards, found
}

func (s staticEndpointIndex) Shardz() map[string]map[string]*EndpointShards {
	out := make(map[string]map[string]*EndpointShards)
	for service, namespaces := range s {
		out[service] = make(map[string]*EndpointShards)
		for namespace, shards := range namespaces {
			out[service][namespace] = shards.DeepCopy()
		}
	}
	return out
}

func TestAggregateEndpointIndex(t *testing.T) {
	kubeShard := ShardKey{Cluster: "cluster1", Provider: provider.Kubernetes}
	externalShard := ShardKey{Cluster: "cluster1", Provider: provider.External}
	mutable := NewMutableEndpointIndex(DisabledCache{})
	mutable.UpdateServiceEndpoints(kubeShard, "svc", "ns", []*IstioEndpoint{
		{Addresses: []string{"10.0.0.1"}, ServicePortName: "http", ServiceAccount: "shared"},
	}, false)
	external := staticEndpointIndex{
		"svc": {
			"ns": {
				Shards: map[ShardKey][]*IstioEndpoint{
					externalShard: {{Addresses: []string{"10.0.0.2"}, ServicePortName: "http", ServiceAccount: "external"}},
				},
				ServiceAccounts: sets.New("shared", "external"),
			},
			"other": {Shards: map[ShardKey][]*IstioEndpoint{}},
		},
	}
	index := NewAggregateEndpointIndex(mutable, external)
	shards, found := index.ShardsForService("svc", "ns")
	assert.Equal(t, found, true)
	assert.Equal(t, shards.Keys(), []ShardKey{externalShard, kubeShard})
	assert.Equal(t, shards.ServiceAccounts, sets.New("shared", "external"))
	assert.Equal(t, len(shards.CopyEndpoints(map[string]int{"http": 80}, sets.New(80))[80]), 2)
	assert.Equal(t, index.Shardz()["svc"]["ns"].Shards, shards.Shards)
	assert.Equal(t, index.Shardz()["svc"]["ns"].ServiceAccounts, shards.ServiceAccounts)

	// A service with no endpoints is distinct from a missing service or namespace.
	_, found = index.ShardsForService("svc", "other")
	assert.Equal(t, found, true)
	for _, key := range [][2]string{{"svc", "missing"}, {"missing", "ns"}} {
		missing, ok := index.ShardsForService(key[0], key[1])
		assert.Equal(t, ok, false)
		assert.Equal(t, missing, nil)
	}

	// Debug output owns its endpoint objects, slices, maps, and account sets.
	dump := index.Shardz()
	dump["svc"]["ns"].Shards[kubeShard][0].Addresses[0] = "changed"
	dump["svc"]["ns"].Shards[externalShard][0].Addresses[0] = "changed"
	dump["svc"]["ns"].ServiceAccounts.Delete("external")
	delete(dump["svc"], "other")
	fresh := index.Shardz()
	assert.Equal(t, fresh["svc"]["ns"].Shards[kubeShard][0].Addresses[0], "10.0.0.1")
	assert.Equal(t, fresh["svc"]["ns"].Shards[externalShard][0].Addresses[0], "10.0.0.2")
	assert.Equal(t, fresh["svc"]["ns"].ServiceAccounts, sets.New("shared", "external"))
	assert.Equal(t, len(fresh["svc"]), 2)

	// Removing the mutable contribution must leave the other source visible.
	mutable.DeleteServiceShard(kubeShard, "svc", "ns", false)
	remaining, found := index.ShardsForService("svc", "ns")
	assert.Equal(t, found, true)
	assert.Equal(t, remaining.Keys(), []ShardKey{externalShard})
	// The earlier aggregate snapshot still owns its shard map and slices.
	assert.Equal(t, shards.Keys(), []ShardKey{externalShard, kubeShard})
}

func TestAggregateEndpointIndexSharedShard(t *testing.T) {
	key := ShardKey{Cluster: "cluster1"}
	source := func(address string) EndpointIndex {
		return staticEndpointIndex{"svc": {"ns": {
			Shards: map[ShardKey][]*IstioEndpoint{key: {{Addresses: []string{address}}}},
		}}}
	}
	index := NewAggregateEndpointIndex(source("10.0.0.1"), source("10.0.0.2"))
	shards, found := index.ShardsForService("svc", "ns")
	assert.Equal(t, found, true)
	assert.Equal(t, len(shards.Shards[key]), 2)
	assert.Equal(t, shards.Shards[key][0].Addresses, []string{"10.0.0.1"})
	assert.Equal(t, shards.Shards[key][1].Addresses, []string{"10.0.0.2"})
	assert.Equal(t, index.Shardz()["svc"]["ns"].Shards, shards.Shards)
	assert.Equal(t, index.Shardz()["svc"]["ns"].ServiceAccounts, shards.ServiceAccounts)
}

func TestAggregateEndpointIndexEmptyAndSingleSource(t *testing.T) {
	empty := NewAggregateEndpointIndex()
	shards, found := empty.ShardsForService("svc", "ns")
	assert.Equal(t, found, false)
	assert.Equal(t, shards, nil)
	assert.Equal(t, len(empty.Shardz()), 0)

	env := NewEnvironment()
	key := ShardKey{Cluster: "cluster1"}
	env.MutableEndpointIndex.UpdateServiceEndpoints(key, "svc", "ns", []*IstioEndpoint{
		{Addresses: []string{"10.0.0.1"}},
	}, false)
	shards, found = env.EndpointIndex.ShardsForService("svc", "ns")
	assert.Equal(t, found, true)
	assert.Equal(t, shards.Shards[key][0].Addresses, []string{"10.0.0.1"})
	env.MutableEndpointIndex.DeleteShard(key)
	_, found = env.EndpointIndex.ShardsForService("svc", "ns")
	assert.Equal(t, found, false)
}
