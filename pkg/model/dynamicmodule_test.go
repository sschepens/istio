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

	udpa "github.com/cncf/xds/go/udpa/type/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	extensions "istio.io/api/extensions/v1alpha1"
	"istio.io/istio/pkg/test/util/assert"
)

func TestDynamicModuleConfigEnvelope(t *testing.T) {
	pluginConfig, err := structpb.NewStruct(map[string]any{"nested": map[string]any{"enabled": true, "values": []any{"one", "two"}}})
	if err != nil {
		t.Fatal(err)
	}
	filterConfig, err := anypb.New(pluginConfig)
	if err != nil {
		t.Fatal(err)
	}
	original := &DynamicModuleConfigEnvelope{
		FilterConfig: filterConfig, PullPolicy: extensions.PullPolicy_Always, PullSecretRequired: true,
		PullSecret: []byte(`{"auths":{"registry":{"auth":"credentials"}}}`), ResourceVersion: "42",
	}
	encoded, err := original.ToAny()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalDynamicModuleConfigEnvelope(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded == nil {
		t.Fatal("missing envelope")
	}
	if !proto.Equal(decoded.FilterConfig, original.FilterConfig) {
		t.Fatalf("config changed: %v", decoded.FilterConfig)
	}
	assert.Equal(t, decoded.PullSecret, original.PullSecret)
	assert.Equal(t, decoded.ResourceVersion, original.ResourceVersion)
	assert.Equal(t, decoded.PullPolicy, original.PullPolicy)
	assert.Equal(t, decoded.PullSecretRequired, original.PullSecretRequired)
}

func TestDynamicModuleConfigEnvelopeInvalid(t *testing.T) {
	unrelated, err := anypb.New(wrapperspb.String("other extension"))
	if err != nil {
		t.Fatal(err)
	}
	for _, config := range []*anypb.Any{nil, unrelated} {
		decoded, err := UnmarshalDynamicModuleConfigEnvelope(config)
		if err != nil || decoded != nil {
			t.Fatalf("unrelated config: %v, %v", decoded, err)
		}
	}
	for _, envelope := range []*DynamicModuleConfigEnvelope{nil, {}} {
		if _, err := envelope.ToAny(); err == nil {
			t.Fatal("accepted missing config")
		}
	}
	for _, data := range []string{`{}`, `{"filterConfig":null}`, `{"filterConfig":{"value":12}}`, `{"filterConfig":{},"pullSecret":"!"}`} {
		t.Run(data, func(t *testing.T) {
			value := &structpb.Struct{}
			if err := value.UnmarshalJSON([]byte(data)); err != nil {
				t.Fatal(err)
			}
			encoded, err := anypb.New(&udpa.TypedStruct{TypeUrl: DynamicModuleTransportType, Value: value})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := UnmarshalDynamicModuleConfigEnvelope(encoded); err == nil {
				t.Fatal("accepted malformed envelope")
			}
		})
	}
}
