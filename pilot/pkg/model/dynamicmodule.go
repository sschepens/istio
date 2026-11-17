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
	"net/url"
	"strings"

	dynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/dynamic_modules/v3"
	httpdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/dynamic_modules/v3"
	networkdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/dynamic_modules/v3"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"

	extensions "istio.io/api/extensions/v1alpha1"
	pm "istio.io/istio/pkg/model"
)

// BuildHTTPDynamicModuleFilter builds an Envoy HTTP DynamicModule filter.
func (e *TrafficExtensionWrapper) BuildHTTPDynamicModuleFilter(pullSecret []byte) (*httpdynamic.DynamicModuleFilter, error) {
	cfg := e.GetDynamicModule()
	if cfg == nil || (cfg.Type != extensions.PluginType_HTTP && cfg.Type != extensions.PluginType_UNSPECIFIED_PLUGIN_TYPE) {
		return nil, nil
	}
	module, filterConfig, err := e.buildDynamicModuleConfig(pullSecret)
	if err != nil {
		return nil, err
	}
	return &httpdynamic.DynamicModuleFilter{DynamicModuleConfig: module, FilterName: cfg.PluginName, FilterConfig: filterConfig}, nil
}

// BuildNetworkDynamicModuleFilter builds an Envoy network DynamicModule filter.
func (e *TrafficExtensionWrapper) BuildNetworkDynamicModuleFilter(pullSecret []byte) (*networkdynamic.DynamicModuleNetworkFilter, error) {
	cfg := e.GetDynamicModule()
	if cfg == nil || cfg.Type != extensions.PluginType_NETWORK {
		return nil, nil
	}
	module, filterConfig, err := e.buildDynamicModuleConfig(pullSecret)
	if err != nil {
		return nil, err
	}
	return &networkdynamic.DynamicModuleNetworkFilter{DynamicModuleConfig: module, FilterName: cfg.PluginName, FilterConfig: filterConfig}, nil
}

// buildDynamicModuleConfig wraps filter_config with agent-only fetch options for remote modules.
func (e *TrafficExtensionWrapper) buildDynamicModuleConfig(pullSecret []byte) (*dynamic.DynamicModuleConfig, *anypb.Any, error) {
	cfg := e.GetDynamicModule()
	moduleURL := cfg.Url
	if !strings.Contains(moduleURL, "://") {
		moduleURL = "oci://" + moduleURL
	}
	u, err := url.Parse(moduleURL)
	if err != nil {
		return nil, nil, err
	}
	pluginConfig := cfg.PluginConfig
	if pluginConfig == nil {
		pluginConfig = &structpb.Struct{}
	}
	filterConfig, err := anypb.New(pluginConfig)
	if err != nil {
		return nil, nil, err
	}
	source := buildDataSource(u, moduleURL, cfg.Sha256)
	if source.GetRemote() != nil {
		envelope := &pm.DynamicModuleConfigEnvelope{
			FilterConfig: filterConfig, PullSecret: pullSecret, PullSecretRequired: cfg.ImagePullSecret != "",
			PullPolicy: cfg.ImagePullPolicy, ResourceVersion: e.ResourceVersion,
		}
		filterConfig, err = envelope.ToAny()
		if err != nil {
			return nil, nil, err
		}
	}
	return &dynamic.DynamicModuleConfig{Module: source}, filterConfig, nil
}
