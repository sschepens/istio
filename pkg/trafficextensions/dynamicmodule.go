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

// convertDynamicModuleFromRemoteToLocal unwraps fetch options and downloads the library for a remote Envoy filter.
// A nil result leaves filters without an agent envelope unchanged.
// Download failures NACK ECDS, preventing Envoy's remote-module fail-open behavior.
func convertDynamicModuleFromRemoteToLocal(ec *core.TypedExtensionConfig, filter proto.Message, cache Cache) (*anypb.Any, error) {
	status := conversionSuccess
	defer func() {
		trafficExtensionConfigConversionCount.With(extensionTypeTag.Value(dynamicModuleExtensionType), resultTag.Value(status)).Increment()
	}()

	var module *dynamic.DynamicModuleConfig
	var filterConfig *anypb.Any
	switch cfg := filter.(type) {
	case *httpdynamic.DynamicModuleFilter:
		module, filterConfig = cfg.DynamicModuleConfig, cfg.FilterConfig
	case *networkdynamic.DynamicModuleNetworkFilter:
		module, filterConfig = cfg.DynamicModuleConfig, cfg.FilterConfig
	default:
		status = unmarshalFailure
		return nil, fmt.Errorf("unsupported DynamicModule filter %T", filter)
	}
	envelope, err := model.UnmarshalDynamicModuleConfigEnvelope(filterConfig)
	if err != nil {
		status = unmarshalFailure
		return nil, err
	}
	if envelope == nil {
		status = noRemoteLoad
		return nil, nil
	}
	remote := module.GetModule().GetRemote()
	uri := remote.GetHttpUri()
	if uri == nil || uri.Uri == "" {
		status = missRemoteFetchHint
		return nil, fmt.Errorf("missing dynamic module remote URI")
	}
	if envelope.PullSecretRequired && len(envelope.PullSecret) == 0 {
		status = fetchFailure
		return nil, fmt.Errorf("missing dynamic module image pull secret")
	}
	timeout := 30 * time.Second
	if uri.Timeout != nil {
		timeout = uri.Timeout.AsDuration()
	}
	path, err := cache.Get(uri.Uri, GetOptions{DynamicModule: true, Checksum: remote.Sha256,
		ResourceName: ec.Name, ResourceVersion: envelope.ResourceVersion, RequestTimeout: timeout,
		PullSecret: envelope.PullSecret, PullPolicy: PullPolicy(envelope.PullPolicy)})
	if err != nil {
		status = fetchFailure
		return nil, fmt.Errorf("cannot fetch dynamic module %s: %w", uri.Uri, err)
	}
	module.Module = &core.AsyncDataSource{Specifier: &core.AsyncDataSource_Local{
		Local: &core.DataSource{Specifier: &core.DataSource_Filename{Filename: path}},
	}}

	switch cfg := filter.(type) {
	case *httpdynamic.DynamicModuleFilter:
		cfg.FilterConfig = envelope.FilterConfig
	case *networkdynamic.DynamicModuleNetworkFilter:
		cfg.FilterConfig = envelope.FilterConfig
	}
	typedConfig, err := anypb.New(filter)
	if err != nil {
		status = marshalFailure
		return nil, err
	}
	ec.TypedConfig = typedConfig
	result, err := anypb.New(ec)
	if err != nil {
		status = marshalFailure
	}
	return result, err
}
