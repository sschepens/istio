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
	"cmp"

	"istio.io/istio/pkg/slices"
	"istio.io/istio/pkg/util/sets"
)

var _ EndpointIndex = (*AggregateEndpointIndex)(nil)

// AggregateEndpointIndex combines reads from a fixed set of endpoint indexes.
// Sources retain responsibility for cache invalidation and push notifications.
// Sources should own disjoint shards. If they contribute to the same shard, their
// endpoints are concatenated in source order without deduplication.
type AggregateEndpointIndex struct {
	indexes []EndpointIndex
}

// NewAggregateEndpointIndex creates a read-only view over the supplied indexes.
// The source list is fixed at construction; updates within each source remain visible.
func NewAggregateEndpointIndex(indexes ...EndpointIndex) *AggregateEndpointIndex {
	return &AggregateEndpointIndex{indexes: slices.Clone(indexes)}
}

// EndpointsForService combines endpoints and tracked accounts from all sources.
// Each source is read independently; the result is not an atomic snapshot across sources.
func (e *AggregateEndpointIndex) EndpointsForService(serviceName, namespace string) (ServiceEndpoints, bool) {
	if len(e.indexes) == 1 {
		return e.indexes[0].EndpointsForService(serviceName, namespace)
	}
	var merged *aggregateServiceEndpoints
	for _, index := range e.indexes {
		endpoints, found := index.EndpointsForService(serviceName, namespace)
		if !found {
			continue
		}
		if merged == nil {
			merged = &aggregateServiceEndpoints{accounts: sets.New[string]()}
		}
		endpoints.ForEachShard(func(key ShardKey, eps []*IstioEndpoint) bool {
			merged.shards = append(merged.shards, endpointShard{key: key, endpoints: slices.Clone(eps)})
			return true
		})
		merged.accounts.Merge(endpoints.CopyServiceAccounts())
	}
	if merged == nil {
		return nil, false
	}
	// Preserve source order for contributions sharing a key, and the global
	// provider/cluster order expected by EDS for all other contributions.
	slices.SortStableFunc(merged.shards, func(a, b endpointShard) int {
		if c := cmp.Compare(a.key.Provider, b.key.Provider); c != 0 {
			return c
		}
		return cmp.Compare(a.key.Cluster, b.key.Cluster)
	})
	combined := merged.shards[:0]
	for _, shard := range merged.shards {
		if len(combined) > 0 && combined[len(combined)-1].key == shard.key {
			last := &combined[len(combined)-1]
			last.endpoints = append(last.endpoints, shard.endpoints...)
		} else {
			combined = append(combined, shard)
		}
	}
	clear(merged.shards[len(combined):])
	merged.shards = combined
	return merged, true
}

type endpointShard struct {
	key       ShardKey
	endpoints []*IstioEndpoint
}

// aggregateServiceEndpoints owns the shard slices and account set collected at lookup.
// Endpoint objects are immutable and shared with the sources.
type aggregateServiceEndpoints struct {
	shards   []endpointShard
	accounts sets.String
}

var _ ServiceEndpoints = (*aggregateServiceEndpoints)(nil)

func (s *aggregateServiceEndpoints) ForEachShard(yield func(ShardKey, []*IstioEndpoint) bool) {
	for _, shard := range s.shards {
		if !yield(shard.key, shard.endpoints) {
			return
		}
	}
}

func (s *aggregateServiceEndpoints) CopyServiceAccounts() sets.String {
	return s.accounts.Copy()
}

// Shardz returns a deep copy of the combined indexes for debugging and tests.
func (e *AggregateEndpointIndex) Shardz() map[string]map[string]*EndpointShards {
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
	if dst.Shards == nil {
		dst.Shards = make(map[ShardKey][]*IstioEndpoint)
	}
	for key, endpoints := range src.Shards {
		dst.Shards[key] = append(dst.Shards[key], endpoints...)
	}
	if dst.ServiceAccounts == nil {
		dst.ServiceAccounts = sets.New[string]()
	}
	dst.ServiceAccounts.Merge(src.ServiceAccounts)
}
