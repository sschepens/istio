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
	"testing"
	"time"

	"istio.io/istio/pkg/kube/krt"
	"istio.io/istio/pkg/ptr"
	"istio.io/istio/pkg/test/util/assert"
)

type statefulInput struct {
	Name       string
	OutputName string
	Dependency string
	Mode       string
	Value      int
}

func (i statefulInput) ResourceName() string { return i.Name }

type statefulValue struct {
	Name  string
	Value int
}

func (v *statefulValue) ResourceName() string { return v.Name }

type statefulCall struct {
	Previous *statefulValue
	Result   *statefulValue
}

func receiveStatefulCall(t *testing.T, calls <-chan statefulCall) statefulCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stateful transformation")
		return statefulCall{}
	}
}

func TestStatefulPointerCollection(t *testing.T) {
	opts := testOptions(t)
	input := krt.NewMutableCollection[statefulInput](nil, nil, opts.WithName("inputs")...)
	calls := make(chan statefulCall, 20)
	output := krt.NewStatefulPointerCollection(input.AsCollection(),
		func(_ krt.HandlerContext, in statefulInput, previous *statefulValue) *statefulValue {
			var result *statefulValue
			if in.Mode != "omit" {
				result = &statefulValue{Name: in.OutputName, Value: in.Value}
				if previous != nil {
					result.Value += previous.Value
				}
			}
			calls <- statefulCall{Previous: previous, Result: result}
			return result
		}, opts.WithName("outputs")...)
	assert.Equal(t, output.WaitUntilSynced(opts.Stop()), true)

	get := func(key string) *statefulValue { return ptr.Flatten(output.GetKey(key)) }
	update := func(in statefulInput, previous *statefulValue) *statefulValue {
		t.Helper()
		input.UpdateObject(in)
		call := receiveStatefulCall(t, calls)
		assert.Equal(t, call.Previous == previous, true, "previous must be the published pointer for this input")
		assert.EventuallyEqual(t, func() *statefulValue { return get(in.OutputName) }, call.Result)
		return call.Result
	}

	first := update(statefulInput{Name: "a", OutputName: "out-a", Value: 1}, nil)
	assert.Equal(t, get("out-a") == first, true, "publish the exact returned pointer")
	second := update(statefulInput{Name: "b", OutputName: "out-b", Value: 10}, nil)
	current := update(statefulInput{Name: "a", OutputName: "out-a", Value: 2}, first)
	assert.Equal(t, current.Value, 3)
	assert.Equal(t, first.Value, 1, "previous output remains immutable")
	assert.Equal(t, get("out-b") == second, true, "another input's state is independent")

	// Equal results retain the published pointer, and the next invocation must receive that
	// pointer rather than the equal object returned by the previous invocation.
	equalResult := update(statefulInput{Name: "a", OutputName: "out-a"}, current)
	assert.Equal(t, equalResult == current, false)
	renamed := update(statefulInput{Name: "a", OutputName: "renamed", Value: 1}, current)
	assert.Equal(t, renamed.Value, 4)
	assert.Equal(t, get("out-a"), nil)

	// Returning nil removes both the output and the previous value passed to the next invocation.
	update(statefulInput{Name: "a", OutputName: "renamed", Mode: "omit"}, renamed)
	recreated := update(statefulInput{Name: "a", OutputName: "out-a", Value: 5}, nil)
	assert.Equal(t, recreated.Value, 5)

	// Primary deletion must not invoke the transformation, and recreation starts without history.
	input.DeleteObject("a")
	assert.EventuallyEqual(t, func() *statefulValue { return get("out-a") }, nil)
	update(statefulInput{Name: "a", OutputName: "out-a", Value: 7}, nil)
	assert.Equal(t, get("out-b") == second, true)
	assert.Equal(t, len(calls), 0)
}

func TestStatefulPointerCollectionDiscardResult(t *testing.T) {
	for _, initialMode := range []string{"discard", "discard-nil"} {
		t.Run(initialMode, func(t *testing.T) {
			opts := testOptions(t)
			input := krt.NewMutableCollection[statefulInput](nil, nil, opts.WithName("inputs")...)
			calls := make(chan statefulCall, 20)
			output := krt.NewStatefulPointerCollection(input.AsCollection(),
				func(ctx krt.HandlerContext, in statefulInput, previous *statefulValue) *statefulValue {
					result := &statefulValue{Name: "output", Value: in.Value}
					if in.Mode != "" {
						ctx.DiscardResult()
						if in.Mode == "discard-nil" {
							result = nil
						}
					}
					calls <- statefulCall{Previous: previous, Result: result}
					return result
				}, opts.WithName("outputs")...)
			assert.Equal(t, output.WaitUntilSynced(opts.Stop()), true)
			get := func() *statefulValue { return ptr.Flatten(output.GetKey("output")) }
			update := func(mode string, value int, previous *statefulValue) statefulCall {
				t.Helper()
				input.UpdateObject(statefulInput{Name: "input", Mode: mode, Value: value})
				call := receiveStatefulCall(t, calls)
				assert.Equal(t, call.Previous == previous, true)
				return call
			}

			initial := update(initialMode, 1, nil)
			// A non-nil initial default is accepted despite DiscardResult; nil stays absent.
			assert.EventuallyEqual(t, get, initial.Result)
			published := update("", 2, initial.Result).Result
			assert.EventuallyEqual(t, get, published)

			update("discard", 3, published)
			// The next invocation observes the retained output, never the discarded result.
			update("discard-nil", 4, published)
			final := update("", 5, published).Result
			assert.EventuallyEqual(t, get, final)
			assert.Equal(t, published.Value, 2)
		})
	}
}

func TestStatefulPointerCollectionDependencies(t *testing.T) {
	opts := testOptions(t)
	input := krt.NewMutableCollection[statefulInput](nil, nil, opts.WithName("inputs")...)
	dependencies := krt.NewMutableCollection(nil, []*statefulValue{
		{Name: "b", Value: 1},
	}, opts.WithName("dependencies")...)
	calls := make(chan statefulCall, 20)
	output := krt.NewStatefulPointerCollection(input.AsCollection(),
		func(ctx krt.HandlerContext, in statefulInput, previous *statefulValue) *statefulValue {
			result := &statefulValue{Name: "output"}
			dependency := ptr.Flatten(krt.FetchOne(ctx, dependencies.AsCollection(), krt.FilterKey(in.Dependency)))
			if dependency != nil {
				result.Value = dependency.Value
			} else if previous != nil {
				result.Value = previous.Value
			}
			calls <- statefulCall{Previous: previous, Result: result}
			return result
		}, opts.WithName("outputs")...)
	assert.Equal(t, output.WaitUntilSynced(opts.Stop()), true)
	get := func() *statefulValue { return ptr.Flatten(output.GetKey("output")) }

	input.UpdateObject(statefulInput{Name: "input", Dependency: "a"})
	initial := receiveStatefulCall(t, calls)
	assert.Equal(t, initial.Previous, nil)
	assert.EventuallyEqual(t, get, &statefulValue{Name: "output"})
	published := get()

	// An initially absent dependency must still be tracked after publishing the empty default.
	dependencies.UpdateObject(&statefulValue{Name: "a", Value: 1})
	added := receiveStatefulCall(t, calls)
	assert.Equal(t, added.Previous == published, true)
	assert.EventuallyEqual(t, get, &statefulValue{Name: "output", Value: 1})
	published = get()

	// Changing the dependency produces an equal output. The old pointer must survive,
	// while the dependency tracking must still switch from a to b.
	input.UpdateObject(statefulInput{Name: "input", Dependency: "b"})
	equal := receiveStatefulCall(t, calls)
	assert.Equal(t, equal.Previous == published, true)
	assert.Equal(t, equal.Result == published, false)
	dependencies.UpdateObject(&statefulValue{Name: "b", Value: 2})
	changed := receiveStatefulCall(t, calls)
	assert.Equal(t, changed.Previous == published, true)
	assert.EventuallyEqual(t, get, &statefulValue{Name: "output", Value: 2})
	published = get()

	// Losing a secondary input retains only the selected value. Its reappearance must
	// still trigger recomputation, even though the empty result compared equal.
	dependencies.DeleteObject("b")
	removed := receiveStatefulCall(t, calls)
	assert.Equal(t, removed.Previous == published, true)
	assert.Equal(t, removed.Result.Value, 2)
	dependencies.UpdateObject(&statefulValue{Name: "b", Value: 3})
	returned := receiveStatefulCall(t, calls)
	assert.Equal(t, returned.Previous == published, true)
	assert.EventuallyEqual(t, get, &statefulValue{Name: "output", Value: 3})
}
