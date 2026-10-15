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

import "istio.io/istio/pkg/util/sets"

// ServiceEndpoints provides read access without prescribing storage or locking.
// Implementations may expose live data or an immutable snapshot. Separate method
// calls are not guaranteed to observe the same version.
type ServiceEndpoints interface {
	// ForEachShard visits each shard once in a stable implementation-defined order
	// and stops when yield returns false. The callback must be short and must not mutate data
	// or re-enter the index, since an implementation may hold a read lock.
	// Endpoint slices are valid only during the callback. Endpoint objects are
	// immutable and may be retained by the caller.
	ForEachShard(yield func(ShardKey, []*IstioEndpoint) bool)
	// ServiceAccounts returns the tracked accounts. The returned set is read-only
	// and may be retained by the caller; callers must copy it before making changes.
	ServiceAccounts() sets.String
}

var (
	_ ServiceEndpoints = (*EndpointShards)(nil)
	_ ServiceEndpoints = SingleShardEndpoints{}
)

// SingleShardEndpoints is an immutable view for a source with one shard, such as
// a KRT service registry. Its fields must not be modified after publication.
type SingleShardEndpoints struct {
	Key       ShardKey
	Endpoints []*IstioEndpoint
	Accounts  sets.String
}

func (s SingleShardEndpoints) ForEachShard(yield func(ShardKey, []*IstioEndpoint) bool) {
	yield(s.Key, s.Endpoints)
}

func (s SingleShardEndpoints) ServiceAccounts() sets.String {
	return s.Accounts
}

// CopyEndpoints takes a snapshot of all endpoints. As input, it takes a map of port name to number, to allow it to group
// the results by service port number. This is a bit weird, but lets us efficiently construct the format the caller needs.
func CopyEndpoints(endpoints ServiceEndpoints, portMap map[string]int, ports sets.Set[int]) map[int][]*IstioEndpoint {
	res := map[int][]*IstioEndpoint{}
	endpoints.ForEachShard(func(_ ShardKey, v []*IstioEndpoint) bool {
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
		return true
	})
	return res
}
