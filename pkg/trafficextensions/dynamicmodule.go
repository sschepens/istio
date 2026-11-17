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

package trafficextensions

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	udpa "github.com/cncf/xds/go/udpa/type/v1"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	dynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/dynamic_modules/v3"
	httpdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/dynamic_modules/v3"
	networkdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/dynamic_modules/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"

	extensions "istio.io/api/extensions/v1alpha1"
	"istio.io/istio/pkg/model"
	"istio.io/istio/pkg/util/protomarshal"
)

// convertDynamicModule consumes the control plane's agent-only download envelope.
// Download failures NACK ECDS, preventing Envoy's remote-module fail-open behavior.
func convertDynamicModule(resource *anypb.Any, cache Cache) (*anypb.Any, bool, error) {
	ec := &core.TypedExtensionConfig{}
	if err := resource.UnmarshalTo(ec); err != nil {
		return nil, false, nil
	}
	envelope := &udpa.TypedStruct{}
	if ec.TypedConfig == nil || !ec.TypedConfig.MessageIs(envelope) {
		return nil, false, nil
	}
	if err := ec.TypedConfig.UnmarshalTo(envelope); err != nil {
		return nil, true, err
	}
	if envelope.TypeUrl != model.DynamicModuleTransportType {
		return nil, false, nil
	}
	if envelope.Value == nil {
		return nil, true, fmt.Errorf("missing dynamic module config")
	}
	version := envelope.Value.Fields["resourceVersion"].GetStringValue()
	delete(envelope.Value.Fields, "resourceVersion")
	data, err := protomarshal.ToJSON(envelope.Value)
	if err != nil {
		return nil, true, err
	}
	cfg := &extensions.DynamicModuleConfig{}
	if err := protomarshal.Unmarshal([]byte(data), cfg); err != nil {
		return nil, true, err
	}
	moduleURL := cfg.Url
	if !strings.Contains(moduleURL, "://") {
		moduleURL = "oci://" + moduleURL
	}
	u, err := url.Parse(moduleURL)
	if err != nil {
		return nil, true, err
	}
	path := strings.TrimPrefix(moduleURL, "file://")
	if u.Scheme != "file" {
		var secret []byte
		if v, found := envelope.Value.Fields["imagePullSecret"]; found {
			if v.GetStringValue() == "" {
				return nil, true, fmt.Errorf("missing dynamic module image pull secret")
			}
			secret = []byte(v.GetStringValue())
		}
		path, err = cache.Get(moduleURL, GetOptions{DynamicModule: true, Checksum: cfg.Sha256,
			ResourceName: ec.Name, ResourceVersion: version, RequestTimeout: 30 * time.Second,
			PullSecret: secret, PullPolicy: PullPolicy(cfg.ImagePullPolicy)})
		if err != nil {
			return nil, true, fmt.Errorf("cannot fetch dynamic module %s: %w", moduleURL, err)
		}
	}
	module := &dynamic.DynamicModuleConfig{Module: &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Local{
		Local: &core.DataSource{Specifier: &core.DataSource_Filename{Filename: path}},
	}}}
	config := cfg.PluginConfig
	if config == nil {
		config = &structpb.Struct{}
	}
	pluginConfig, err := anypb.New(config)
	if err != nil {
		return nil, true, err
	}
	var filter proto.Message = &httpdynamic.DynamicModuleFilter{DynamicModuleConfig: module, FilterName: cfg.PluginName, FilterConfig: pluginConfig}
	if cfg.Type == extensions.PluginType_NETWORK {
		filter = &networkdynamic.DynamicModuleNetworkFilter{DynamicModuleConfig: module, FilterName: cfg.PluginName, FilterConfig: pluginConfig}
	}
	ec.TypedConfig, err = anypb.New(filter)
	if err != nil {
		return nil, true, err
	}
	result, err := anypb.New(ec)
	return result, true, err
}
