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
	"fmt"
	"net/url"
	"strings"

	dynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/dynamic_modules/v3"
	httpdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/dynamic_modules/v3"
	networkdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/dynamic_modules/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"

	extensions "istio.io/api/extensions/v1alpha1"
	pm "istio.io/istio/pkg/model"
)

// BuildDynamicModuleFilter builds a standard Envoy filter with agent-only fetch options in filter_config.
func (e *TrafficExtensionWrapper) BuildDynamicModuleFilter(pullSecret []byte) (proto.Message, error) {
	cfg := e.GetDynamicModule()
	if cfg == nil {
		return nil, fmt.Errorf("missing dynamic module config")
	}
	moduleURL := cfg.Url
	if !strings.Contains(moduleURL, "://") {
		moduleURL = "oci://" + moduleURL
	}
	u, err := url.Parse(moduleURL)
	if err != nil {
		return nil, err
	}
	pluginConfig := cfg.PluginConfig
	if pluginConfig == nil {
		pluginConfig = &structpb.Struct{}
	}
	filterConfig, err := anypb.New(pluginConfig)
	if err != nil {
		return nil, err
	}
	envelope := &pm.DynamicModuleConfigEnvelope{
		FilterConfig:       filterConfig,
		PullSecret:         pullSecret,
		PullSecretRequired: cfg.ImagePullSecret != "",
		PullPolicy:         cfg.ImagePullPolicy,
		ResourceVersion:    e.ResourceVersion,
	}
	wrappedConfig, err := envelope.ToAny()
	if err != nil {
		return nil, err
	}
	module := &dynamic.DynamicModuleConfig{
		Module:          buildDataSource(u, moduleURL, cfg.Sha256),
		NackOnCacheMiss: true,
	}
	if cfg.Type == extensions.PluginType_NETWORK {
		return &networkdynamic.DynamicModuleNetworkFilter{DynamicModuleConfig: module, FilterName: cfg.PluginName, FilterConfig: wrappedConfig}, nil
	}
	return &httpdynamic.DynamicModuleFilter{DynamicModuleConfig: module, FilterName: cfg.PluginName, FilterConfig: wrappedConfig}, nil
}
