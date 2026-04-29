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

package krt_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/test/util/assert"
)

// fakeExternalSource is a hand-written ExternalSource used to drive bridge tests.
// It tracks registration / unregistration and lets tests fire events explicitly.
type fakeExternalSource struct {
	mu sync.Mutex

	items map[string]Named

	handler    func(krt.Event[Named])
	registered bool
	unregister atomic.Int32

	syncedCh chan struct{}
}

func newFakeSource(initial ...Named) *fakeExternalSource {
	f := &fakeExternalSource{
		items:    map[string]Named{},
		syncedCh: make(chan struct{}),
	}
	for _, n := range initial {
		f.items[n.ResourceName()] = n
	}
	return f
}

func (f *fakeExternalSource) HasSynced() bool {
	select {
	case <-f.syncedCh:
		return true
	default:
		return false
	}
}

func (f *fakeExternalSource) WaitUntilSynced(stop <-chan struct{}) bool {
	select {
	case <-f.syncedCh:
		return true
	case <-stop:
		return false
	}
}

func (f *fakeExternalSource) markSynced() {
	close(f.syncedCh)
}

func (f *fakeExternalSource) List() []Named {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Named, 0, len(f.items))
	for _, v := range f.items {
		out = append(out, v)
	}
	return out
}

func (f *fakeExternalSource) GetKey(k string) *Named {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.items[k]; ok {
		return &v
	}
	return nil
}

func (f *fakeExternalSource) Register(h func(krt.Event[Named])) func() {
	f.mu.Lock()
	if f.registered {
		f.mu.Unlock()
		panic("fakeExternalSource only supports one subscription")
	}
	f.registered = true
	f.handler = h
	f.mu.Unlock()
	return func() {
		f.unregister.Add(1)
	}
}

// fire updates internal state and dispatches the resulting event to the bridge.
// State is updated under the lock BEFORE the callback fires, matching the
// ExternalSource contract.
func (f *fakeExternalSource) fire(ev krt.Event[Named]) {
	f.mu.Lock()
	switch ev.Event {
	case controllers.EventAdd, controllers.EventUpdate:
		f.items[krt.GetKey(*ev.New)] = *ev.New
	case controllers.EventDelete:
		delete(f.items, krt.GetKey(*ev.Old))
	}
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		h(ev)
	}
}

func (f *fakeExternalSource) add(n Named) {
	f.fire(krt.Event[Named]{New: &n, Event: controllers.EventAdd})
}

func (f *fakeExternalSource) update(oldN, newN Named) {
	f.fire(krt.Event[Named]{Old: &oldN, New: &newN, Event: controllers.EventUpdate})
}

func (f *fakeExternalSource) del(n Named) {
	f.fire(krt.Event[Named]{Old: &n, Event: controllers.EventDelete})
}

func assertPanics(t *testing.T, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic, got none")
		}
	}()
	f()
}

func TestExternalCollection_BasicReadthrough(t *testing.T) {
	src := newFakeSource(Named{"ns", "a"}, Named{"ns", "b"})
	src.markSynced()
	opts := testOptions(t)
	c := krt.WrapExternalSource(src, opts.WithName("ext")...)

	assert.Equal(t, c.HasSynced(), true)
	assert.Equal(t, len(c.List()), 2)
	assert.Equal(t, *c.GetKey("ns/a"), Named{"ns", "a"})
	assert.Equal(t, c.GetKey("missing"), nil)
}

func TestExternalCollection_EventForwarding(t *testing.T) {
	src := newFakeSource()
	src.markSynced()
	opts := testOptions(t)
	c := krt.WrapExternalSource(src, opts.WithName("ext")...)

	tt := assert.NewTracker[string](t)
	c.RegisterBatch(BatchedTrackerHandler[Named](tt), false)

	src.add(Named{"ns", "a"})
	tt.WaitOrdered("add/ns/a")

	src.update(Named{"ns", "a"}, Named{"ns", "a"})
	tt.WaitOrdered("update/ns/a")

	src.del(Named{"ns", "a"})
	tt.WaitOrdered("delete/ns/a")

	// Source state must reflect events already delivered.
	assert.Equal(t, c.GetKey("ns/a"), nil)
}

func TestExternalCollection_MultipleHandlersFanOut(t *testing.T) {
	src := newFakeSource()
	src.markSynced()
	opts := testOptions(t)
	c := krt.WrapExternalSource(src, opts.WithName("ext")...)

	t1 := assert.NewTracker[string](t)
	t2 := assert.NewTracker[string](t)
	c.RegisterBatch(BatchedTrackerHandler[Named](t1), false)
	c.RegisterBatch(BatchedTrackerHandler[Named](t2), false)

	src.add(Named{"ns", "a"})
	t1.WaitOrdered("add/ns/a")
	t2.WaitOrdered("add/ns/a")
}

func TestExternalCollection_RegisterBatchRunExistingStateRejected(t *testing.T) {
	src := newFakeSource(Named{"ns", "a"})
	src.markSynced()
	opts := testOptions(t)
	c := krt.WrapExternalSource(src, opts.WithName("ext")...)

	assertPanics(t, func() {
		c.RegisterBatch(func([]krt.Event[Named]) {}, true)
	})
}

func TestExternalCollection_RegisterRejected(t *testing.T) {
	// Register routes through registerHandlerAsBatched, which hardcodes
	// runExistingState=true and is therefore unsupported.
	src := newFakeSource()
	src.markSynced()
	opts := testOptions(t)
	c := krt.WrapExternalSource(src, opts.WithName("ext")...)

	assertPanics(t, func() {
		c.Register(func(krt.Event[Named]) {})
	})
}

func TestExternalCollection_NewIndexRejected(t *testing.T) {
	src := newFakeSource()
	src.markSynced()
	opts := testOptions(t)
	c := krt.WrapExternalSource(src, opts.WithName("ext")...)

	assertPanics(t, func() {
		krt.NewIndex(c, "by-ns", func(n Named) []string {
			return []string{n.Namespace}
		})
	})
}

func TestExternalCollection_NotSyncedUntilSourceSynced(t *testing.T) {
	src := newFakeSource()
	opts := testOptions(t)
	c := krt.WrapExternalSource(src, opts.WithName("ext")...)

	assert.Equal(t, c.HasSynced(), false)

	// WaitUntilSynced must return promptly when stop fires before sync.
	stop := make(chan struct{})
	close(stop)
	assert.Equal(t, c.WaitUntilSynced(stop), false)

	// After source signals synced, the collection reports synced.
	src.markSynced()
	assert.Equal(t, c.HasSynced(), true)
}

func TestExternalCollection_StopUnregistersSource(t *testing.T) {
	src := newFakeSource()
	src.markSynced()
	stop := make(chan struct{})
	_ = krt.WrapExternalSource(src, krt.WithStop(stop), krt.WithName("ext"))

	assert.Equal(t, src.unregister.Load(), int32(0))
	close(stop)
	assert.EventuallyEqual(t, src.unregister.Load, int32(1))
}

func TestExternalCollection_FetchFromTransformation(t *testing.T) {
	// External collections are intended to be used as Fetch dependencies from
	// transformations. Drive a NewCollection whose primary input is a static
	// collection and whose secondary dependency is the external collection.
	src := newFakeSource(Named{"ns", "ext-a"})
	src.markSynced()

	opts := testOptions(t)
	ext := krt.WrapExternalSource(src, opts.WithName("ext")...)

	primary := krt.NewStaticCollection[Named](nil, []Named{{"ns", "p"}}, opts.WithName("primary")...)

	derived := krt.NewCollection(primary, func(ctx krt.HandlerContext, in Named) *Named {
		seen := krt.Fetch(ctx, ext)
		// Mark the input with a count of how many externs are visible. Using the
		// resource name lets the test assert without exposing more fields.
		out := Named{Namespace: in.Namespace, Name: fmt.Sprintf("%s:%d", in.Name, len(seen))}
		return &out
	}, opts.WithName("derived")...)

	assert.EventuallyEqual(t, func() []Named {
		return derived.List()
	}, []Named{{"ns", "p:1"}})

	// Adding to the external collection re-runs the transformation.
	src.add(Named{"ns", "ext-b"})
	assert.EventuallyEqual(t, func() []Named {
		return derived.List()
	}, []Named{{"ns", "p:2"}})
}

func TestExternalCollection_ConcurrentEvents(t *testing.T) {
	src := newFakeSource()
	src.markSynced()
	opts := testOptions(t)
	c := krt.WrapExternalSource(src, opts.WithName("ext")...)

	const writers = 8
	const perWriter = 50
	delivered := atomic.Int64{}
	c.RegisterBatch(func(evs []krt.Event[Named]) {
		delivered.Add(int64(len(evs)))
	}, false)

	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		w := w
		go func() {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				src.add(Named{Namespace: "ns", Name: fmt.Sprintf("w%d-%d", w, i)})
			}
		}()
	}
	wg.Wait()

	assert.EventuallyEqual(t, func() int64 {
		return delivered.Load()
	}, int64(writers*perWriter))
	assert.Equal(t, len(c.List()), writers*perWriter)
}

