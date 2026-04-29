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

package krt

import (
	"fmt"

	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/slices"
)

// ExternalSource is the contract a non-krt collection must satisfy to be wrapped by
// WrapExternalSource. The source is the authoritative store for state; the bridge
// only forwards events.
//
// The resulting Collection is a leaf in the krt graph. It supports being read from a
// transformation via Fetch and being subscribed to via RegisterBatch(_, false), but it
// does NOT support:
//   - Being a primary input to NewCollection / NewManyCollection (those bootstrap with
//     RegisterBatch(_, true), which we cannot satisfy without an atomic snapshot of the
//     source).
//   - Being a member of Join / MergeJoin / NestedJoin (same reason).
//   - Being wrapped by NewIndex (krt-side indexes are not maintained on the bridge).
//   - The non-batched Register helper (it routes through registerHandlerAsBatched, which
//     hardcodes runExistingState=true; consumers must use RegisterBatch(f, false)).
//
// Implementation contract for the source:
//   - List/GetKey return the source's current state. They MUST reflect the effects of
//     any event already delivered to the registered handler (i.e. update internal state
//     before invoking the callback, not after). Downstream krt handlers re-Fetch from
//     the source on each event, so violating this leads to stale derived state.
//   - Events MAY be delivered concurrently, but doing so for the same key risks
//     downstream observers seeing events out of source-order. Prefer serial delivery,
//     or partition concurrent delivery by key.
//   - The source itself is a Syncer; HasSynced / WaitUntilSynced report when initial
//     population is complete.
type ExternalSource[T any] interface {
	// Syncer reports when the source has finished its own initial population
	// (whatever that means for the source). The bridge surfaces these methods on
	// the resulting Collection.
	Syncer

	// List returns the current contents of the source. Order is undefined. The result
	// must reflect every event already delivered to the registered handler.
	List() []T

	// GetKey returns the object stored under k, or nil if absent. As with List, the
	// result must reflect every event already delivered to the registered handler.
	GetKey(k string) *T

	// Register subscribes handler to changes from the source. Unlike a Kubernetes
	// informer, the source is NOT required to replay existing items as EventAdd on
	// subscription; the bridge does not surface initial state to its consumers and
	// rejects RegisterBatch(_, true), so any replay would be wasted work.
	//
	// The returned func detaches the handler. The bridge invokes it when the
	// collection's stop channel fires.
	Register(handler func(o Event[T])) func()
}

type externalCollection[T any] struct {
	source         ExternalSource[T]
	collectionName string
	id             collectionUID
	augmentation   func(a any) any
	metadata       Metadata

	eventHandlers *handlerSet[T]
	stop          <-chan struct{}
}

var _ internalCollection[any] = &externalCollection[any]{}

// WrapExternalSource wraps an arbitrary ExternalSource into a krt Collection.
// A single subscription is established with the source; events are fanned out to all
// downstream krt consumers via the standard handlerSet.
func WrapExternalSource[T any](source ExternalSource[T], opts ...CollectionOption) Collection[T] {
	o := buildCollectionOptions(opts...)
	if o.name == "" {
		o.name = fmt.Sprintf("External[%v]", ptr.TypeName[T]())
	}

	e := &externalCollection[T]{
		source:         source,
		collectionName: o.name,
		id:             nextUID(),
		augmentation:   o.augmentation,
		metadata:       o.metadata,
		eventHandlers:  newHandlerSet[T](),
		stop:           o.stop,
	}

	unregister := source.Register(e.onEvent)

	go func() {
		<-o.stop
		unregister()
	}()

	maybeRegisterCollectionForDebugging[T](e, o.debugger)
	return e
}

// onEvent is the single sink for events from the source. Concurrent invocations are
// safe: handlerSet.Distribute serializes its own internal state and each downstream
// listener has its own queue. See the type docstring for ordering caveats when the
// source emits concurrently for the same key.
func (e *externalCollection[T]) onEvent(ev Event[T]) {
	e.eventHandlers.Distribute([]Event[T]{ev}, !e.HasSynced())
}

// GetKey delegates to the source. The result is whatever the source currently holds
// for k, including any state changes the source has already announced via onEvent.
func (e *externalCollection[T]) GetKey(k string) *T { return e.source.GetKey(k) }

// List delegates to the source and returns its full current contents. Order is
// undefined.
func (e *externalCollection[T]) List() []T { return e.source.List() }

// Metadata returns the metadata associated with this collection at construction time.
func (e *externalCollection[T]) Metadata() Metadata { return e.metadata }

// Register is unsupported: the helper it would normally route through
// (registerHandlerAsBatched) hardcodes runExistingState=true, which RegisterBatch
// rejects. Consumers must call RegisterBatch(f, false) directly.
func (e *externalCollection[T]) Register(f func(o Event[T])) HandlerRegistration {
	return registerHandlerAsBatched[T](e, f)
}

// RegisterBatch subscribes f to events forwarded from the source. runExistingState
// must be false; the bridge cannot atomically snapshot the source while inserting
// the handler, so any List()-based replay would race against events arriving via
// onEvent.
func (e *externalCollection[T]) RegisterBatch(f func(o []Event[T]), runExistingState bool) HandlerRegistration {
	if runExistingState {
		panic(fmt.Sprintf("runExistingState is not supported on external collection %q", e.collectionName))
	}
	return e.eventHandlers.Insert(f, e.source, nil, e.stop)
}

// Synced returns the source itself, which implements Syncer.
func (e *externalCollection[T]) Synced() Syncer { return e.source }

// HasSynced reports whether the source has finished its initial population.
func (e *externalCollection[T]) HasSynced() bool { return e.source.HasSynced() }

// WaitUntilSynced blocks until the source reports synced or stop fires. Returns
// true on synced, false on stop.
func (e *externalCollection[T]) WaitUntilSynced(stop <-chan struct{}) bool {
	return e.source.WaitUntilSynced(stop)
}

// name returns the human-facing name of the collection.
// nolint: unused // (not true, its to implement an interface)
func (e *externalCollection[T]) name() string { return e.collectionName }

// uid returns the globally unique internal id of the collection.
// nolint: unused // (not true, its to implement an interface)
func (e *externalCollection[T]) uid() collectionUID { return e.id }

// augment applies the optional WithObjectAugmentation transform to a, or returns a
// unchanged when no augmentation is configured.
// nolint: unused // (not true, its to implement an interface)
func (e *externalCollection[T]) augment(a any) any {
	if e.augmentation != nil {
		return e.augmentation(a)
	}
	return a
}

// dump returns a snapshot of the source's current contents for debug purposes. The
// snapshot reflects source state, not a bridge-side cache.
// nolint: unused // (not true, its to implement an interface)
func (e *externalCollection[T]) dump() CollectionDump {
	return CollectionDump{
		Outputs: eraseMap(slices.GroupUnique(e.List(), getTypedKey)),
		Synced:  e.HasSynced(),
	}
}

// index is unsupported: the bridge does not maintain krt-side secondary indexes, so
// callers cannot wrap an external collection with NewIndex.
// nolint: unused // (not true, its to implement an interface)
func (e *externalCollection[T]) index(name string, extract func(o T) []string) indexer[T] {
	panic(fmt.Sprintf("indexes are not supported on external collection %q", e.collectionName))
}
