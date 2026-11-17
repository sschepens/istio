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
	"encoding/json"
	"fmt"

	udpa "github.com/cncf/xds/go/udpa/type/v1"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"

	extensions "istio.io/api/extensions/v1alpha1"
)

const (
	// DynamicModuleTransportType identifies the agent-only ECDS download envelope.
	DynamicModuleTransportType     = APITypePrefix + "istio.agent.DynamicModule"
	DynamicModuleHTTPFilterType    = APITypePrefix + "envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter"
	DynamicModuleNetworkFilterType = APITypePrefix + "envoy.extensions.filters.network.dynamic_modules.v3.DynamicModuleNetworkFilter"
)

// DynamicModuleConfigEnvelope wraps the plugin configuration with agent-only fetch options.
// It is carried in the Envoy DynamicModule filter's filter_config field and removed by the agent.
type DynamicModuleConfigEnvelope struct {
	FilterConfig       *anypb.Any            `json:"filterConfig"`
	PullSecret         []byte                `json:"pullSecret,omitempty"`
	PullSecretRequired bool                  `json:"pullSecretRequired,omitempty"`
	PullPolicy         extensions.PullPolicy `json:"pullPolicy,omitempty"`
	ResourceVersion    string                `json:"resourceVersion,omitempty"`
}

// ToAny encodes the envelope as a TypedStruct for transport over ECDS.
func (e *DynamicModuleConfigEnvelope) ToAny() (*anypb.Any, error) {
	if e == nil || e.FilterConfig == nil {
		return nil, fmt.Errorf("missing dynamic module filter config")
	}
	data, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	value := &structpb.Struct{}
	if err := value.UnmarshalJSON(data); err != nil {
		return nil, err
	}
	return anypb.New(&udpa.TypedStruct{TypeUrl: DynamicModuleTransportType, Value: value})
}

// UnmarshalDynamicModuleConfigEnvelope decodes an agent-only envelope.
// It returns nil without an error for configurations of other types.
func UnmarshalDynamicModuleConfigEnvelope(config *anypb.Any) (*DynamicModuleConfigEnvelope, error) {
	transport := &udpa.TypedStruct{}
	if config == nil || !config.MessageIs(transport) {
		return nil, nil
	}
	if err := config.UnmarshalTo(transport); err != nil {
		return nil, err
	}
	if transport.TypeUrl != DynamicModuleTransportType {
		return nil, nil
	}
	if transport.Value == nil {
		return nil, fmt.Errorf("missing dynamic module envelope")
	}
	data, err := transport.Value.MarshalJSON()
	if err != nil {
		return nil, err
	}
	envelope := &DynamicModuleConfigEnvelope{}
	if err := json.Unmarshal(data, envelope); err != nil {
		return nil, err
	}
	if envelope.FilterConfig == nil {
		return nil, fmt.Errorf("missing dynamic module filter config")
	}
	return envelope, nil
}
