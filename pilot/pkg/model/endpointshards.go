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
	// Protects shards and serviceAccounts.
	mu sync.RWMutex

	// shards is used to track the shards. EDS updates are grouped by shard.
	// Current implementation uses the registry name as key - in multicluster this is the
	// name of the k8s cluster, derived from the config (secret).
	shards map[ShardKey][]*IstioEndpoint

	// serviceAccounts has the concatenation of all service accounts seen so far in endpoints.
	// This is updated on push, based on shards. If the previous list is different than
	// current list, a full push will be forced, to trigger a secure naming update.
	// Due to the larger time, it is still possible that connection errors will occur while
	// CDS is updated.
	// Published sets must be replaced rather than modified in place, since
	// ServiceAccounts returns them directly to callers.
	serviceAccounts sets.String
}

// EndpointShardsDump is an exclusively owned debug snapshot of a service's endpoints.
// Its maps, slices, endpoint objects, and service accounts may be modified by the caller.
type EndpointShardsDump struct {
	Shards          map[ShardKey][]*IstioEndpoint
	ServiceAccounts sets.String
}

// keys gives a sorted list of keys for EndpointShards.shards.
// Calls to keys should be guarded with a lock on the EndpointShards.
func (es *EndpointShards) keys() []ShardKey {
	// len(shards) ~= number of remote clusters which isn't too large, doing this sort frequently
	// shouldn't be too problematic. If it becomes an issue we can cache it in the EndpointShards struct.
	keys := make([]ShardKey, 0, len(es.shards))
	for k := range es.shards {
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

// ForEachShard visits shards in provider/cluster order while holding the read lock.
func (es *EndpointShards) ForEachShard(yield func(ShardKey, []*IstioEndpoint) bool) {
	es.mu.RLock()
	defer es.mu.RUnlock()
	if len(es.shards) <= 1 {
		for key, endpoints := range es.shards {
			yield(key, endpoints)
		}
		return
	}
	for _, key := range es.keys() {
		if !yield(key, es.shards[key]) {
			return
		}
	}
}

// ServiceAccounts returns the tracked service accounts. The returned set is read-only
// and remains valid across updates; callers must copy it before making changes.
func (es *EndpointShards) ServiceAccounts() sets.String {
	es.mu.RLock()
	defer es.mu.RUnlock()
	return es.serviceAccounts
}

// snapshot returns an exclusively owned debug snapshot under a single read lock.
func (es *EndpointShards) snapshot() *EndpointShardsDump {
	es.mu.RLock()
	defer es.mu.RUnlock()
	res := &EndpointShardsDump{
		Shards:          make(map[ShardKey][]*IstioEndpoint, len(es.shards)),
		ServiceAccounts: es.serviceAccounts.Copy(),
	}
	for k, v := range es.shards {
		res.Shards[k] = make([]*IstioEndpoint, len(v))
		for i, ep := range v {
			res.Shards[k][i] = ep.DeepCopy()
		}
	}
	return res
}

// EndpointIndex provides read access to endpoint shards across service registries.
// Implementations manage synchronization internally; returned endpoints are read-only.
type EndpointIndex interface {
	EndpointsForService(serviceName, namespace string) (ServiceEndpoints, bool)
	// Shardz returns a deep copy for debugging and tests. Callers may modify it.
	// Implementations can use SnapshotServiceEndpoints to construct each service's snapshot.
	Shardz() map[string]map[string]*EndpointShardsDump
}

var _ EndpointIndex = (*MutableEndpointIndex)(nil)

// MutableEndpointIndex is a mutex protected index populated by incremental registry updates.
// Mutation and the associated cache invalidation remain local to this implementation.
type MutableEndpointIndex struct {
	mu sync.RWMutex
	// keyed by svc then ns
	shardsBySvc map[string]map[string]*EndpointShards
	// We'll need to clear the cache in-sync with endpoint shards modifications.
	cache XdsCache
}

// NewMutableEndpointIndex creates a mutable endpoint index.
func NewMutableEndpointIndex(cache XdsCache) *MutableEndpointIndex {
	return &MutableEndpointIndex{
		shardsBySvc: make(map[string]map[string]*EndpointShards),
		cache:       cache,
	}
}

// must be called with lock
func (e *MutableEndpointIndex) clearCacheForService(svc, ns string) {
	e.cache.Clear(sets.Set[ConfigKey]{{
		Kind:      kind.ServiceEntry,
		Name:      svc,
		Namespace: ns,
	}: {}})
}

// Shardz returns a full deep copy of the global map of shards. This should be used only for testing
// and debugging, as the cloning is expensive.
func (e *MutableEndpointIndex) Shardz() map[string]map[string]*EndpointShardsDump {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make(map[string]map[string]*EndpointShardsDump, len(e.shardsBySvc))
	for svcKey, v := range e.shardsBySvc {
		out[svcKey] = make(map[string]*EndpointShardsDump, len(v))
		for nsKey, v := range v {
			out[svcKey][nsKey] = v.snapshot()
		}
	}
	return out
}

// EndpointsForService returns a synchronized view of the service's endpoints.
func (e *MutableEndpointIndex) EndpointsForService(serviceName, namespace string) (ServiceEndpoints, bool) {
	shards, found := e.shardsForService(serviceName, namespace)
	if !found {
		return nil, false
	}
	return shards, true
}

func (e *MutableEndpointIndex) shardsForService(serviceName, namespace string) (*EndpointShards, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	byNs, ok := e.shardsBySvc[serviceName]
	if !ok {
		return nil, false
	}
	shards, ok := byNs[namespace]
	return shards, ok
}

// getOrCreateEndpointShard returns the shards. The second return parameter will be true if this service was seen
// for the first time.
func (e *MutableEndpointIndex) getOrCreateEndpointShard(serviceName, namespace string) (*EndpointShards, bool) {
	// attempt to find endpoint shard with a read lock first, to avoid unnecessary lock contention. If not found, acquire a write lock and create it.
	if ep, ok := e.shardsForService(serviceName, namespace); ok {
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
			shards:          map[ShardKey][]*IstioEndpoint{},
			serviceAccounts: sets.String{},
		}
		m[namespace] = ep
		// Clear the cache here to avoid race in cache writes.
		e.clearCacheForService(serviceName, namespace)
	}

	return ep, true
}

func (e *MutableEndpointIndex) DeleteServiceShard(shard ShardKey, serviceName, namespace string, preserveKeys bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deleteServiceInner(shard, serviceName, namespace, preserveKeys)
}

func (e *MutableEndpointIndex) DeleteShard(shardKey ShardKey) {
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
func (e *MutableEndpointIndex) PruneShard(shardKey ShardKey, keep map[string]sets.String) {
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
func (e *MutableEndpointIndex) deleteServiceInner(shard ShardKey, serviceName, namespace string, preserveKeys bool) {
	if e.shardsBySvc[serviceName] == nil ||
		e.shardsBySvc[serviceName][namespace] == nil {
		return
	}
	epShards := e.shardsBySvc[serviceName][namespace]
	epShards.mu.Lock()
	delete(epShards.shards, shard)
	// Clear the cache here to avoid race in cache writes.
	e.clearCacheForService(serviceName, namespace)
	if !preserveKeys {
		if len(epShards.shards) == 0 {
			delete(e.shardsBySvc[serviceName], namespace)
		}
		if len(e.shardsBySvc[serviceName]) == 0 {
			delete(e.shardsBySvc, serviceName)
		}
	}
	epShards.mu.Unlock()
}

// PushType is an enumeration that decides what type push we should do when we get EDS update.
type PushType int

const (
	// NoPush does not push any thing.
	NoPush PushType = iota
	// IncrementalPush just pushes endpoints.
	IncrementalPush
	// FullPush triggers full push - used when endpoint changes require regenerating more than EDS,
	// such as a change to the set of service accounts backing a service.
	FullPush
)

// UpdateServiceEndpoints updates EndpointShards data by clusterID, hostname, IstioEndpoints.
// It also tracks the changes to ServiceAccounts. It returns whether endpoints need to be pushed and
// it also returns if they need to be pushed whether a full push is needed or incremental push is sufficient.
func (e *MutableEndpointIndex) UpdateServiceEndpoints(
	shard ShardKey,
	hostname string,
	namespace string,
	istioEndpoints []*IstioEndpoint,
	logPushType bool,
) PushType {
	if len(istioEndpoints) == 0 {
		// Should delete the service EndpointShards when endpoints become zero to prevent memory leak,
		// but we should not delete the keys from EndpointIndex map - that will trigger
		// unnecessary full push which can become a real problem if a pod is in crashloop and thus endpoints
		// flip flopping between 1 and 0.
		e.DeleteServiceShard(shard, hostname, namespace, true)
		if logPushType {
			log.Infof("Incremental push, service %s at shard %v has no endpoints", hostname, shard)
		} else {
			log.Infof("Cache Update, Service %s at shard %v has no endpoints", hostname, shard)
		}
		return IncrementalPush
	}

	pushType := IncrementalPush
	// Find endpoint shard for this service, if it is available - otherwise create a new one.
	// Creating a shard means we have not seen endpoints for this service before, but it does not
	// warrant a full push. Endpoints are recorded below regardless of the push type, and registries
	// trigger their own service update (a kind.ServiceEntry ConfigUpdate, see initRegistryEventHandlers)
	// whenever a service is added, which re-initializes the service registry and picks these endpoints
	// up. If the service is not known yet, an endpoint-only push is simply a no-op until it is.
	ep, _ := e.getOrCreateEndpointShard(hostname, namespace)

	ep.mu.Lock()
	defer ep.mu.Unlock()
	oldIstioEndpoints := ep.shards[shard]
	newIstioEndpoints, needPush := endpointUpdateRequiresPush(oldIstioEndpoints, istioEndpoints)

	if !needPush {
		log.Debugf("No push, either old endpoint health status did not change or new endpoint came with unhealthy status, %v", hostname)
		pushType = NoPush
	}

	ep.shards[shard] = newIstioEndpoints

	// Check if ServiceAccounts have changed. We should do a full push if they have changed.
	saUpdated := updateShardServiceAccount(ep, hostname)

	// For existing endpoints, we need to do full push if service accounts change.
	if saUpdated {
		if logPushType {
			log.Infof("Full push, service accounts changed, %v", hostname)
		} else {
			log.Infof("Cache Update, service accounts changed, %v", hostname)
		}
		pushType = FullPush
	}

	// Clear the cache here. While it would likely be cleared later when we trigger a push, a race
	// condition is introduced where an XDS response may be generated before the update, but not
	// completed until after a response after the update. Essentially, we transition from v0 -> v1 ->
	// v0 -> invalidate -> v1. Reverting a change we pushed violates our contract of monotonically
	// moving forward in version. In practice, this is pretty rare and self corrects nearly
	// immediately. However, clearing the cache here has almost no impact on cache performance as we
	// would clear it shortly after anyways.
	e.clearCacheForService(hostname, namespace)

	return pushType
}

// endpointUpdateRequiresPush determines if an endpoint update is required.
func endpointUpdateRequiresPush(oldIstioEndpoints []*IstioEndpoint, incomingEndpoints []*IstioEndpoint) ([]*IstioEndpoint, bool) {
	if oldIstioEndpoints == nil {
		// If there are no old endpoints, we should push with incoming endpoints as there is nothing to compare.
		return incomingEndpoints, true
	}
	needPush := false
	newIstioEndpoints := make([]*IstioEndpoint, 0, len(incomingEndpoints))
	// Check if new Endpoints are ready to be pushed. This check
	// will ensure that if a new pod comes with a non ready endpoint,
	// we do not unnecessarily push that config to Envoy.
	omap := make(map[string]*IstioEndpoint, len(oldIstioEndpoints))
	nmap := make(map[string]*IstioEndpoint, len(newIstioEndpoints))
	// Add new endpoints only if they are ever ready once to shards
	// so that full push does not send them from shards.
	for _, oie := range oldIstioEndpoints {
		omap[oie.Key()] = oie
	}
	for _, nie := range incomingEndpoints {
		nmap[nie.Key()] = nie
	}
	for _, nie := range incomingEndpoints {
		if oie, exists := omap[nie.Key()]; exists {
			// If endpoint exists already, we should push if it's changed.
			// Skip this check if we already decide we need to push to avoid expensive checks
			if !needPush && !oie.Equals(nie) {
				needPush = true
			}
			newIstioEndpoints = append(newIstioEndpoints, nie)
		} else {
			// If the endpoint does not exist in shards that means it is a
			// new endpoint. Always send new healthy endpoints.
			// Also send new unhealthy endpoints when SendUnhealthyEndpoints is enabled.
			// This is OK since we disable panic threshold when SendUnhealthyEndpoints is enabled.
			if nie.HealthStatus != UnHealthy || nie.SendUnhealthyEndpoints {
				needPush = true
			}
			newIstioEndpoints = append(newIstioEndpoints, nie)
		}
	}
	// Next, check for endpoints that were in old but no longer exist. If there are any, there is a
	// removal so we need to push an update.
	if !needPush {
		for _, oie := range oldIstioEndpoints {
			if _, f := nmap[oie.Key()]; !f {
				needPush = true
				break
			}
		}
	}

	return newIstioEndpoints, needPush
}

// updateShardServiceAccount updates the service endpoints' sa when service/endpoint event happens.
// Note: it is not concurrent safe.
func updateShardServiceAccount(shards *EndpointShards, serviceName string) bool {
	oldServiceAccount := shards.serviceAccounts
	serviceAccounts := sets.String{}
	for _, epShards := range shards.shards {
		for _, ep := range epShards {
			if ep.ServiceAccount != "" {
				serviceAccounts.Insert(ep.ServiceAccount)
			}
		}
	}

	if !oldServiceAccount.Equals(serviceAccounts) {
		shards.serviceAccounts = serviceAccounts
		log.Debugf("Updating service accounts now, svc %v, before service account %v, after %v",
			serviceName, oldServiceAccount, serviceAccounts)
		return true
	}

	return false
}
