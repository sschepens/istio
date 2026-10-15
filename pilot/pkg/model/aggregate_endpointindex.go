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

// ShardsForService merges the shards and service accounts for a service.
// Each source is read independently; the result is not an atomic snapshot across sources.
func (e *AggregateEndpointIndex) ShardsForService(serviceName, namespace string) (*EndpointShards, bool) {
	// Preserve the mutable index read path, including its locking and allocation behavior,
	// while it is the only source.
	if len(e.indexes) == 1 {
		return e.indexes[0].ShardsForService(serviceName, namespace)
	}
	var merged *EndpointShards
	for _, index := range e.indexes {
		shards, found := index.ShardsForService(serviceName, namespace)
		if !found {
			continue
		}
		if merged == nil {
			merged = &EndpointShards{
				Shards:          make(map[ShardKey][]*IstioEndpoint),
				ServiceAccounts: sets.New[string](),
			}
		}
		mergeEndpointShards(merged, shards)
	}
	return merged, merged != nil
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

// mergeEndpointShards copies source map entries and slices into a private destination.
// Endpoint objects remain shared; readers must not mutate them.
func mergeEndpointShards(dst, src *EndpointShards) {
	src.RLock()
	defer src.RUnlock()
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
