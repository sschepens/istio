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

	"istio.io/istio/pilot/pkg/features"
	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/sets"
)

var _ EndpointIndex = (*AggregateEndpointIndex)(nil)

// AggregateEndpointIndex combines reads from a fixed set of endpoint indexes.
// Sources retain responsibility for cache invalidation and push notifications.
// Sources must own disjoint shards. Iteration follows source order, preserving
// each source's own shard order.
type AggregateEndpointIndex struct {
	indexes []EndpointIndex
}

// NewAggregateEndpointIndex creates a read-only view over the supplied indexes.
// The source list is fixed at construction; updates within each source remain visible.
func NewAggregateEndpointIndex(indexes ...EndpointIndex) *AggregateEndpointIndex {
	return &AggregateEndpointIndex{indexes: slices.Clone(indexes)}
}

// EndpointsForService returns a view over the matching sources without copying endpoints.
// Each source manages its own synchronization and determines whether its view is live or immutable.
func (e *AggregateEndpointIndex) EndpointsForService(serviceName, namespace string) (ServiceEndpoints, bool) {
	if len(e.indexes) == 1 {
		return e.indexes[0].EndpointsForService(serviceName, namespace)
	}
	var sources aggregateServiceEndpoints
	for _, index := range e.indexes {
		if endpoints, found := index.EndpointsForService(serviceName, namespace); found {
			sources = append(sources, endpoints)
		}
	}
	switch len(sources) {
	case 0:
		return nil, false
	case 1:
		return sources[0], true
	default:
		return sources, true
	}
}

// aggregateServiceEndpoints delegates reads to the views obtained at lookup.
type aggregateServiceEndpoints []ServiceEndpoints

var _ ServiceEndpoints = aggregateServiceEndpoints(nil)

func (s aggregateServiceEndpoints) ForEachShard(yield func(ShardKey, []*IstioEndpoint) bool) {
	if features.EnableUnsafeAssertions {
		seen := sets.New[ShardKey]()
		next := yield
		yield = func(key ShardKey, endpoints []*IstioEndpoint) bool {
			if seen.Contains(key) {
				panic(fmt.Sprintf("duplicate endpoint shard %s across aggregate sources", key))
			}
			seen.Insert(key)
			return next(key, endpoints)
		}
	}
	keepGoing := true
	visit := func(key ShardKey, endpoints []*IstioEndpoint) bool {
		keepGoing = yield(key, endpoints)
		return keepGoing
	}
	for _, source := range s {
		source.ForEachShard(visit)
		if !keepGoing {
			return
		}
	}
}

func (s aggregateServiceEndpoints) ServiceAccounts() sets.String {
	accounts := sets.New[string]()
	for _, source := range s {
		accounts.Merge(source.ServiceAccounts())
	}
	return accounts
}

// Shardz returns a deep copy of the combined indexes for debugging and tests.
func (e *AggregateEndpointIndex) Shardz() map[string]map[string]*EndpointShards {
	if len(e.indexes) == 1 {
		return e.indexes[0].Shardz()
	}
	out := make(map[string]map[string]*EndpointShards)
	for _, index := range e.indexes {
		for service, byNamespace := range index.Shardz() {
			if out[service] == nil {
				out[service] = make(map[string]*EndpointShards)
			}
			for namespace, shards := range byNamespace {
				if existing := out[service][namespace]; existing != nil {
					mergeEndpointShards(existing, shards)
				} else {
					out[service][namespace] = shards
				}
			}
		}
	}
	return out
}

// mergeEndpointShards combines exclusively owned debug copies.
func mergeEndpointShards(dst, src *EndpointShards) {
	if dst.shards == nil {
		dst.shards = make(map[ShardKey][]*IstioEndpoint)
	}
	for key, endpoints := range src.shards {
		if features.EnableUnsafeAssertions {
			if _, found := dst.shards[key]; found {
				panic(fmt.Sprintf("duplicate endpoint shard %s across aggregate sources", key))
			}
		}
		dst.shards[key] = endpoints
	}
	if dst.serviceAccounts == nil {
		dst.serviceAccounts = sets.New[string]()
	}
	dst.serviceAccounts.Merge(src.serviceAccounts)
}
