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
	"time"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	dynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/dynamic_modules/v3"
	httpdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/dynamic_modules/v3"
	networkdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/dynamic_modules/v3"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"istio.io/istio/pkg/model"
)

// convertDynamicModule unwraps agent-only fetch options from a standard Envoy filter.
// Download failures NACK ECDS, preventing Envoy's remote-module fail-open behavior.
func convertDynamicModule(resource *anypb.Any, cache Cache) (*anypb.Any, bool, error) {
	ec := &core.TypedExtensionConfig{}
	if err := resource.UnmarshalTo(ec); err != nil {
		return nil, false, nil
	}
	var filter proto.Message
	var module *dynamic.DynamicModuleConfig
	var filterConfig **anypb.Any
	switch ec.GetTypedConfig().GetTypeUrl() {
	case model.DynamicModuleHTTPFilterType:
		cfg := &httpdynamic.DynamicModuleFilter{}
		if err := ec.TypedConfig.UnmarshalTo(cfg); err != nil {
			return nil, true, err
		}
		filter, module, filterConfig = cfg, cfg.DynamicModuleConfig, &cfg.FilterConfig
	case model.DynamicModuleNetworkFilterType:
		cfg := &networkdynamic.DynamicModuleNetworkFilter{}
		if err := ec.TypedConfig.UnmarshalTo(cfg); err != nil {
			return nil, true, err
		}
		filter, module, filterConfig = cfg, cfg.DynamicModuleConfig, &cfg.FilterConfig
	default:
		return nil, false, nil
	}
	envelope, err := model.UnmarshalDynamicModuleConfigEnvelope(*filterConfig)
	if err != nil {
		return nil, true, err
	}
	if envelope == nil {
		return nil, false, nil
	}
	source := module.GetModule()
	if remote := source.GetRemote(); remote != nil {
		uri := remote.GetHttpUri()
		if uri == nil || uri.Uri == "" {
			return nil, true, fmt.Errorf("missing dynamic module remote URI")
		}
		if envelope.PullSecretRequired && len(envelope.PullSecret) == 0 {
			return nil, true, fmt.Errorf("missing dynamic module image pull secret")
		}
		timeout := 30 * time.Second
		if uri.Timeout != nil {
			timeout = uri.Timeout.AsDuration()
		}
		path, err := cache.Get(uri.Uri, GetOptions{DynamicModule: true, Checksum: remote.Sha256,
			ResourceName: ec.Name, ResourceVersion: envelope.ResourceVersion, RequestTimeout: timeout,
			PullSecret: envelope.PullSecret, PullPolicy: PullPolicy(envelope.PullPolicy)})
		if err != nil {
			return nil, true, fmt.Errorf("cannot fetch dynamic module %s: %w", uri.Uri, err)
		}
		module.Module = &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Local{
			Local: &core.DataSource{Specifier: &core.DataSource_Filename{Filename: path}},
		}}
	} else if source.GetLocal().GetFilename() == "" {
		return nil, true, fmt.Errorf("missing dynamic module source")
	}
	*filterConfig = envelope.FilterConfig
	ec.TypedConfig, err = anypb.New(filter)
	if err != nil {
		return nil, true, err
	}
	result, err := anypb.New(ec)
	return result, true, err
}
