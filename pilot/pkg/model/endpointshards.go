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
	"sort"
	"sync"

	"istio.io/istio/pilot/pkg/serviceregistry/provider"
	"istio.io/istio/pkg/cluster"
	"istio.io/istio/pkg/config/schema/kind"
	"istio.io/istio/pkg/util/sets"
)

// shardRegistry is a simplified interface for registries that can produce a shard key
type shardRegistry interface {
	Cluster() cluster.ID
	Provider() provider.ID
}

// ShardKeyFromRegistry computes the shard key based on provider type and cluster id.
func ShardKeyFromRegistry(instance shardRegistry) ShardKey {
	return ShardKey{Cluster: instance.Cluster(), Provider: instance.Provider()}
}

// ShardKey is the key for EndpointShards made of a key with the format "provider/cluster"
type ShardKey struct {
	Cluster  cluster.ID
	Provider provider.ID
}

func (sk ShardKey) String() string {
	return string(sk.Provider) + "/" + string(sk.Cluster) // format: %s/%s
}

// MarshalText implements the TextMarshaler interface (for json key usage)
func (sk ShardKey) MarshalText() (text []byte, err error) {
	return []byte(sk.String()), nil
}

// EndpointShards holds the set of endpoint shards of a service. Registries update
// individual shards incrementally. The shards are aggregated and split into
// clusters when a push for the specific cluster is needed.
type EndpointShards struct {
	// mutex protecting below map.
	sync.RWMutex

	// Shards is used to track the shards. EDS updates are grouped by shard.
	// Current implementation uses the registry name as key - in multicluster this is the
	// name of the k8s cluster, derived from the config (secret).
	Shards map[ShardKey][]*IstioEndpoint

	// ServiceAccounts has the concatenation of all service accounts seen so far in endpoints.
	// This is updated on push, based on shards. If the previous list is different than
	// current list, a full push will be forced, to trigger a secure naming update.
	// Due to the larger time, it is still possible that connection errors will occur while
	// CDS is updated.
	ServiceAccounts sets.String
}

// Keys gives a sorted list of keys for EndpointShards.Shards.
// Calls to Keys should be guarded with a lock on the EndpointShards.
func (es *EndpointShards) Keys() []ShardKey {
	// len(shards) ~= number of remote clusters which isn't too large, doing this sort frequently
	// shouldn't be too problematic. If it becomes an issue we can cache it in the EndpointShards struct.
	keys := make([]ShardKey, 0, len(es.Shards))
	for k := range es.Shards {
		keys = append(keys, k)
	}
	if len(keys) >= 2 {
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].Provider == keys[j].Provider {
				return keys[i].Cluster < keys[j].Cluster
			}
			return keys[i].Provider < keys[j].Provider
		})
	}
	return keys
}

// CopyEndpoints takes a snapshot of all endpoints. As input, it takes a map of port name to number, to allow it to group
// the results by service port number. This is a bit weird, but lets us efficiently construct the format the caller needs.
func (es *EndpointShards) CopyEndpoints(portMap map[string]int, ports sets.Set[int]) map[int][]*IstioEndpoint {
	es.RLock()
	defer es.RUnlock()
	res := map[int][]*IstioEndpoint{}
	for _, v := range es.Shards {
		for _, ep := range v {
			// use the port name as the key, unless LegacyClusterPortKey is set and takes precedence
			// In EDS we match on port *name*. But for historical reasons, we match on port number for CDS.
			var portNum int
			if ep.LegacyClusterPortKey != 0 {
				if !ports.Contains(ep.LegacyClusterPortKey) {
					continue
				}
				portNum = ep.LegacyClusterPortKey
			} else {
				pn, f := portMap[ep.ServicePortName]
				if !f {
					continue
				}
				portNum = pn
			}
			res[portNum] = append(res[portNum], ep)
		}
	}
	return res
}

func (es *EndpointShards) DeepCopy() *EndpointShards {
	es.RLock()
	defer es.RUnlock()
	res := &EndpointShards{
		Shards:          make(map[ShardKey][]*IstioEndpoint, len(es.Shards)),
		ServiceAccounts: es.ServiceAccounts.Copy(),
	}
	for k, v := range es.Shards {
		res.Shards[k] = make([]*IstioEndpoint, 0, len(v))
		for _, ep := range v {
			res.Shards[k] = append(res.Shards[k], ep.DeepCopy())
		}
	}
	return res
}

// EndpointIndex is a mutex protected index of endpoint shards
type EndpointIndex struct {
	mu sync.RWMutex
	// keyed by svc then ns
	shardsBySvc map[string]map[string]*EndpointShards
	// We'll need to clear the cache in-sync with endpoint shards modifications.
	cache XdsCache
}

func NewEndpointIndex(cache XdsCache) *EndpointIndex {
	return &EndpointIndex{
		shardsBySvc: make(map[string]map[string]*EndpointShards),
		cache:       cache,
	}
}

// must be called with lock
func (e *EndpointIndex) clearCacheForService(svc, ns string) {
	e.cache.Clear(sets.Set[ConfigKey]{{
		Kind:      kind.ServiceEntry,
		Name:      svc,
		Namespace: ns,
	}: {}})
}

// Shardz returns a full deep copy of the global map of shards. This should be used only for testing
// and debugging, as the cloning is expensive.
func (e *EndpointIndex) Shardz() map[string]map[string]*EndpointShards {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]map[string]*EndpointShards, len(e.shardsBySvc))
	for svcKey, v := range e.shardsBySvc {
		out[svcKey] = make(map[string]*EndpointShards, len(v))
		for nsKey, v := range v {
			out[svcKey][nsKey] = v.DeepCopy()
		}
	}
	return out
}

// ShardsForService returns the shards and true if they are found, or returns nil, false.
func (e *EndpointIndex) ShardsForService(serviceName, namespace string) (*EndpointShards, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	byNs, ok := e.shardsBySvc[serviceName]
	if !ok {
		return nil, false
	}
	shards, ok := byNs[namespace]
	return shards, ok
}

// GetOrCreateEndpointShard returns the shards. The second return parameter will be true if this service was seen
// for the first time.
func (e *EndpointIndex) GetOrCreateEndpointShard(serviceName, namespace string) (*EndpointShards, bool) {
	// attempt to find endpoint shard with a read lock first, to avoid unnecessary lock contention. If not found, acquire a write lock and create it.
	if ep, ok := e.ShardsForService(serviceName, namespace); ok {
		return ep, false
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	m, ok := e.shardsBySvc[serviceName]
	if !ok {
		m = map[string]*EndpointShards{}
		e.shardsBySvc[serviceName] = m
	}

	ep, ok := m[namespace]
	if !ok {
		ep = &EndpointShards{
			Shards:          map[ShardKey][]*IstioEndpoint{},
			ServiceAccounts: sets.String{},
		}
		m[namespace] = ep
		// Clear the cache here to avoid race in cache writes.
		e.clearCacheForService(serviceName, namespace)
	}

	return ep, true
}

func (e *EndpointIndex) DeleteServiceShard(shard ShardKey, serviceName, namespace string, preserveKeys bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deleteServiceInner(shard, serviceName, namespace, preserveKeys)
}

func (e *EndpointIndex) DeleteShard(shardKey ShardKey) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for svc, shardsByNamespace := range e.shardsBySvc {
		for ns := range shardsByNamespace {
			e.deleteServiceInner(shardKey, svc, ns, false)
		}
	}
	if e.cache == nil {
		return
	}
	e.cache.ClearAll()
}

// PruneShard removes shardKey from every (service, namespace) entry not present in keep. A
// registry calls this after completing a full resync (e.g. following a credential rotation) to
// clean up shard entries for services that no longer exist in the source: such a service never
// generates a delete event of its own, since a full resync only lists what currently exists -
// there's nothing left to diff a fresh list against.
func (e *EndpointIndex) PruneShard(shardKey ShardKey, keep map[string]sets.String) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for svc, shardsByNamespace := range e.shardsBySvc {
		for ns := range shardsByNamespace {
			if keep[svc].Contains(ns) {
				continue
			}
			e.deleteServiceInner(shardKey, svc, ns, false)
		}
	}
}

// must be called with lock
func (e *EndpointIndex) deleteServiceInner(shard ShardKey, serviceName, namespace string, preserveKeys bool) {
	if e.shardsBySvc[serviceName] == nil ||
		e.shardsBySvc[serviceName][namespace] == nil {
		return
	}
	epShards := e.shardsBySvc[serviceName][namespace]
	epShards.Lock()
	delete(epShards.Shards, shard)
	// Clear the cache here to avoid race in cache writes.
	e.clearCacheForService(serviceName, namespace)
	if !preserveKeys {
		if len(epShards.Shards) == 0 {
			delete(e.shardsBySvc[serviceName], namespace)
		}
		if len(e.shardsBySvc[serviceName]) == 0 {
			delete(e.shardsBySvc, serviceName)
		}
	}
	epShards.Unlock()
}

// PushType is an enumeration that decides what type push we should do when we get EDS update.
type PushType int

const (
	// NoPush does not push any thing.
	NoPush PushType = iota
	// IncrementalPush just pushes endpoints.
	IncrementalPush
	// FullPush triggers full push - typically used for new services.
	FullPush
)

// UpdateServiceEndpoints updates EndpointShards data by clusterID, hostname, IstioEndpoints.
// It also tracks the changes to ServiceAccounts. It returns whether endpoints need to be pushed and
// it also returns if they need to be pushed whether a full push is needed or incremental push is sufficient.
//
// The index takes ownership of istioEndpoints; callers must not mutate the slice afterwards.
func (e *EndpointIndex) UpdateServiceEndpoints(
	shard ShardKey,
	hostname string,
	namespace string,
	istioEndpoints []*IstioEndpoint,
) PushType {
	if len(istioEndpoints) == 0 {
		// Should delete the service EndpointShards when endpoints become zero to prevent memory leak,
		// but we should not delete the keys from EndpointIndex map - that will trigger
		// unnecessary full push which can become a real problem if a pod is in crashloop and thus endpoints
		// flip flopping between 1 and 0.
		e.DeleteServiceShard(shard, hostname, namespace, true)
		log.Infof("Incremental push, service %s at shard %v has no endpoints", hostname, shard)
		return IncrementalPush
	}

	pushType := IncrementalPush
	// Find endpoint shard for this service, if it is available - otherwise create a new one.
	ep, created := e.GetOrCreateEndpointShard(hostname, namespace)
	// If we create a new endpoint shard, that means we have not seen the service earlier. We should do a full push.
	if created {
		log.Infof("Full push, new service %s/%s", namespace, hostname)
		pushType = FullPush
	}

	ep.Lock()
	defer ep.Unlock()
	if pushType != FullPush && !endpointUpdateRequiresPush(ep.Shards[shard], istioEndpoints) {
		log.Debugf("No push, either old endpoint health status did not change or new endpoint came with unhealthy status, %v", hostname)
		pushType = NoPush
	}

	// For existing endpoints, we need to do full push if service accounts change.
	if saUpdated := e.setShardEndpoints(ep, shard, hostname, namespace, istioEndpoints); saUpdated && pushType != FullPush {
		// Avoid extra logging if already a full push
		log.Infof("Full push, service accounts changed, %v", hostname)
		pushType = FullPush
	}

	return pushType
}

// OverwriteServiceEndpoints replaces the contents of a single shard with istioEndpoints, without
// computing whether the change warrants a push. Callers that do not act on the PushType - because
// they trigger their own push, or none at all - should prefer this over UpdateServiceEndpoints: the
// per-endpoint diff it skips dominates the cost of updating a large service.
//
// The index takes ownership of istioEndpoints; callers must not mutate the slice afterwards.
func (e *EndpointIndex) OverwriteServiceEndpoints(
	shard ShardKey,
	hostname string,
	namespace string,
	istioEndpoints []*IstioEndpoint,
) {
	if len(istioEndpoints) == 0 {
		// As in UpdateServiceEndpoints, drop the shard but keep the keys in the index.
		e.DeleteServiceShard(shard, hostname, namespace, true)
		log.Infof("Cache Update, Service %s at shard %v has no endpoints", hostname, shard)
		return
	}

	ep, created := e.GetOrCreateEndpointShard(hostname, namespace)
	if created {
		log.Infof("Cache Update, new service %s/%s", namespace, hostname)
	}

	ep.Lock()
	defer ep.Unlock()
	e.setShardEndpoints(ep, shard, hostname, namespace, istioEndpoints)
}

// setShardEndpoints stores istioEndpoints as the contents of shard and refreshes the state derived
// from it: the service account set and the XDS cache. It reports whether the service accounts
// changed. Must be called with ep's write lock held.
func (e *EndpointIndex) setShardEndpoints(
	ep *EndpointShards,
	shard ShardKey,
	hostname string,
	namespace string,
	istioEndpoints []*IstioEndpoint,
) bool {
	ep.Shards[shard] = istioEndpoints

	// Check if ServiceAccounts have changed. We should do a full push if they have changed.
	saUpdated := updateShardServiceAccount(ep, hostname)

	// Clear the cache here. While it would likely be cleared later when we trigger a push, a race
	// condition is introduced where an XDS response may be generated before the update, but not
	// completed until after a response after the update. Essentially, we transition from v0 -> v1 ->
	// v0 -> invalidate -> v1. Reverting a change we pushed violates our contract of monotonically
	// moving forward in version. In practice, this is pretty rare and self corrects nearly
	// immediately. However, clearing the cache here has almost no impact on cache performance as we
	// would clear it shortly after anyways.
	e.clearCacheForService(hostname, namespace)

	return saUpdated
}

// endpointUpdateRequiresPush determines if an endpoint update is required.
func endpointUpdateRequiresPush(oldIstioEndpoints []*IstioEndpoint, incomingEndpoints []*IstioEndpoint) bool {
	if oldIstioEndpoints == nil {
		// If there are no old endpoints, we should push with incoming endpoints as there is nothing to compare.
		return true
	}
	omap := make(map[string]*IstioEndpoint, len(oldIstioEndpoints))
	for _, oie := range oldIstioEndpoints {
		omap[oie.Key()] = oie
	}
	// Check if new Endpoints are ready to be pushed. This check
	// will ensure that if a new pod comes with a non ready endpoint,
	// we do not unnecessarily push that config to Envoy.
	for _, nie := range incomingEndpoints {
		if oie, exists := omap[nie.Key()]; exists {
			// If endpoint exists already, we should push if it's changed.
			if !oie.Equals(nie) {
				return true
			}
		} else {
			// If the endpoint does not exist in shards that means it is a
			// new endpoint. Always send new healthy endpoints.
			// Also send new unhealthy endpoints when SendUnhealthyEndpoints is enabled.
			// This is OK since we disable panic threshold when SendUnhealthyEndpoints is enabled.
			if nie.HealthStatus != UnHealthy || nie.SendUnhealthyEndpoints {
				return true
			}
		}
	}
	// Next, check for endpoints that were in old but no longer exist. If there are any, there is a
	// removal so we need to push an update.
	nkeys := sets.NewWithLength[string](len(incomingEndpoints))
	for _, nie := range incomingEndpoints {
		nkeys.Insert(nie.Key())
	}
	for _, oie := range oldIstioEndpoints {
		if !nkeys.Contains(oie.Key()) {
			return true
		}
	}

	return false
}

// updateShardServiceAccount updates the service endpoints' sa when service/endpoint event happens.
// Note: it is not concurrent safe.
func updateShardServiceAccount(shards *EndpointShards, serviceName string) bool {
	oldServiceAccount := shards.ServiceAccounts
	serviceAccounts := sets.String{}
	for _, epShards := range shards.Shards {
		for _, ep := range epShards {
			if ep.ServiceAccount != "" {
				serviceAccounts.Insert(ep.ServiceAccount)
			}
		}
	}

	if !oldServiceAccount.Equals(serviceAccounts) {
		shards.ServiceAccounts = serviceAccounts
		log.Debugf("Updating service accounts now, svc %v, before service account %v, after %v",
			serviceName, oldServiceAccount, serviceAccounts)
		return true
	}

	return false
}
