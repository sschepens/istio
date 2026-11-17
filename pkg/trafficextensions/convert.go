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
	"strings"
	"sync"
	"time"

	udpa "github.com/cncf/xds/go/udpa/type/v1"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	rbacv3 "github.com/envoyproxy/go-control-plane/envoy/config/rbac/v3"
	httpdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/dynamic_modules/v3"
	httprbac "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/rbac/v3"
	httpwasm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/wasm/v3"
	networkdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/dynamic_modules/v3"
	networkrbac "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/rbac/v3"
	networkwasm "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/wasm/v3"
	wasmextensions "github.com/envoyproxy/go-control-plane/envoy/extensions/wasm/v3"
	"github.com/hashicorp/go-multierror"
	"google.golang.org/protobuf/proto"
	anypb "google.golang.org/protobuf/types/known/anypb"

	"istio.io/istio/pkg/bootstrap"
	"istio.io/istio/pkg/model"
	"istio.io/istio/pkg/util/istiomultierror"
	"istio.io/istio/pkg/util/protomarshal"
)

const (
	// DefaultAllowStatPrefix is the stat prefix used for WASM default allow RBAC filters
	DefaultAllowStatPrefix = "wasm-default-allow"
	// DefaultDenyStatPrefix is the stat prefix used for WASM default deny RBAC filters
	DefaultDenyStatPrefix = "wasm-default-deny"
)

var (
	allowHTTPTypedConfig, _ = anypb.New(&httprbac.RBAC{
		// no rules mean allow all.
		RulesStatPrefix: DefaultAllowStatPrefix,
	})

	denyHTTPTypedConfig, _ = anypb.New(&httprbac.RBAC{
		// empty rule means deny all.
		Rules:           &rbacv3.RBAC{},
		RulesStatPrefix: DefaultDenyStatPrefix,
	})

	allowNetworkTypeConfig, _ = anypb.New(&networkrbac.RBAC{
		// no rules mean allow all.
		StatPrefix: DefaultAllowStatPrefix,
	})
	denyNetworkTypedConfig, _ = anypb.New(&networkrbac.RBAC{
		// empty rule means deny all.
		Rules:      &rbacv3.RBAC{},
		StatPrefix: DefaultDenyStatPrefix,
	})
)

func createHTTPDefaultFilter(name string, failOpen bool) (*anypb.Any, error) {
	var tc *anypb.Any
	if failOpen {
		tc = allowHTTPTypedConfig
	} else {
		tc = denyHTTPTypedConfig
	}
	ec := &core.TypedExtensionConfig{
		Name:        name,
		TypedConfig: tc,
	}
	return anypb.New(ec)
}

func createNetworkDefaultFilter(name string, failOpen bool) (*anypb.Any, error) {
	var tc *anypb.Any
	if failOpen {
		tc = allowNetworkTypeConfig
	} else {
		tc = denyNetworkTypedConfig
	}
	ec := &core.TypedExtensionConfig{
		Name:        name,
		TypedConfig: tc,
	}
	return anypb.New(ec)
}

// MaybeConvertExtensionConfig downloads remote Wasm and DynamicModule binaries and
// rewrites their extension configurations to reference local files.
func MaybeConvertExtensionConfig(resources []*anypb.Any, cache Cache) error {
	var wg sync.WaitGroup

	numResources := len(resources)
	convertErrs := make([]error, numResources)
	wg.Add(numResources)

	for i := 0; i < numResources; i++ {
		go func(i int) {
			defer wg.Done()
			startTime := time.Now()
			parsed, extensionType, err := tryUnmarshal(resources[i])
			if extensionType != "" {
				defer func() {
					trafficExtensionConfigConversionDuration.With(extensionTypeTag.Value(extensionType)).Record(float64(time.Since(startTime).Milliseconds()))
				}()
			}
			if err != nil {
				if extensionType != "" {
					trafficExtensionConfigConversionCount.With(extensionTypeTag.Value(extensionType), resultTag.Value(unmarshalFailure)).Increment()
				}
				convertErrs[i] = err
				return
			}

			if parsed == nil {
				// No supported extension requires agent conversion.
				if extensionType != "" {
					trafficExtensionConfigConversionCount.With(extensionTypeTag.Value(extensionType), resultTag.Value(noRemoteLoad)).Increment()
				}
				return
			}
			extConfig := parsed.config
			switch cfg := parsed.filter.(type) {
			case *httpwasm.Wasm:
				newExtensionConfig, err := convertHTTPWasmConfigFromRemoteToLocal(extConfig, cfg, cache)
				if err != nil {
					failOpen := httpWasmFailOpen(cfg)
					rbacFilter := "deny"
					if failOpen {
						rbacFilter = "allow"
					}
					trafficextensionLog.Errorf("error in converting the wasm config to local: %v. applying %s RBAC filter", err, rbacFilter)
					// Use NOOP filter because the download failed.
					// nolint: staticcheck // FailOpen deprecated
					newExtensionConfig, err = createHTTPDefaultFilter(extConfig.GetName(), failOpen)
					if err != nil {
						// If the fallback is failing, send the Nack regardless of fail_open.
						err = fmt.Errorf("failed to create allow-all filter as a fallback of %s Wasm Module: %w", extConfig.GetName(), err)
						convertErrs[i] = err
						return
					}
				}
				resources[i] = newExtensionConfig
			case *networkwasm.Wasm:
				newExtensionConfig, err := convertNetworkWasmConfigFromRemoteToLocal(extConfig, cfg, cache)
				if err != nil {
					failOpen := networkWasmFailOpen(cfg)
					rbacFilter := "deny"
					if failOpen {
						rbacFilter = "allow"
					}
					trafficextensionLog.Errorf("error in converting the wasm config to local: %v. applying %s RBAC filter", err, rbacFilter)

					// Use NOOP filter because the download failed.
					newExtensionConfig, err = createNetworkDefaultFilter(extConfig.GetName(), failOpen)
					if err != nil {
						// If the fallback is failing, send the Nack regardless of fail_open.
						err = fmt.Errorf("failed to create allow-all filter as a fallback of %s Wasm Module: %w", extConfig.GetName(), err)
						convertErrs[i] = err
						return
					}
				}
				resources[i] = newExtensionConfig
			case *httpdynamic.DynamicModuleFilter, *networkdynamic.DynamicModuleNetworkFilter:
				converted, err := convertDynamicModuleFromRemoteToLocal(extConfig, cfg, cache)
				if err != nil {
					trafficextensionLog.Errorf("error in converting the dynamic module config to local: %v", err)
					convertErrs[i] = err
					return
				}
				if converted != nil {
					resources[i] = converted
				}
			}
		}(i)
	}

	wg.Wait()
	err := multierror.Append(istiomultierror.New(), convertErrs...).ErrorOrNil()
	if err != nil {
		trafficextensionLog.Errorf("error converting extension config: %v", err)
	}
	return err
}

func httpWasmFailOpen(http *httpwasm.Wasm) bool {
	if failurePolicy := http.GetConfig().FailurePolicy; failurePolicy != wasmextensions.FailurePolicy_UNSPECIFIED {
		return failurePolicy == wasmextensions.FailurePolicy_FAIL_OPEN
	}

	// FailOpen deprecated
	return http.GetConfig().GetFailOpen() // nolint: staticcheck
}

func networkWasmFailOpen(network *networkwasm.Wasm) bool {
	if failurePolicy := network.GetConfig().FailurePolicy; failurePolicy != wasmextensions.FailurePolicy_UNSPECIFIED {
		return failurePolicy == wasmextensions.FailurePolicy_FAIL_OPEN
	}

	// FailOpen deprecated
	return network.GetConfig().GetFailOpen() // nolint: staticcheck
}

// extensionConfig pairs the decoded ECDS resource with its filter.
type extensionConfig struct {
	config *core.TypedExtensionConfig
	filter proto.Message
}

// tryUnmarshal decodes a supported ECDS resource and selects filters with remote code.
// Local filters and unrelated extensions return nil and are passed through.
// The extension type is retained for metrics on local filters and decoding failures;
// an empty type means the resource could not be identified as a supported extension.
func tryUnmarshal(resource *anypb.Any) (*extensionConfig, string, error) {
	ec := &core.TypedExtensionConfig{}
	if err := resource.UnmarshalTo(ec); err != nil {
		return nil, "", fmt.Errorf("failed to unmarshal extension config resource: %w", err)
	}
	typedConfig := ec.GetTypedConfig()
	if typedConfig == nil {
		return nil, "", fmt.Errorf("typed extension config %+v does not contain any typed config", ec)
	}
	var extensionType string
	var filter proto.Message
	var err error
	if typedConfig.TypeUrl == model.TypedStructType {
		filter, err = unmarshalWasmTypedStruct(typedConfig)
		if filter != nil {
			extensionType = wasmExtensionType
		}
	} else {
		switch typedConfig.TypeUrl {
		case model.WasmHTTPFilterType:
			filter = &httpwasm.Wasm{}
			extensionType = wasmExtensionType
		case model.WasmNetworkFilterType:
			filter = &networkwasm.Wasm{}
			extensionType = wasmExtensionType
		case model.DynamicModuleHTTPFilterType:
			filter = &httpdynamic.DynamicModuleFilter{}
			extensionType = dynamicModuleExtensionType
		case model.DynamicModuleNetworkFilterType:
			filter = &networkdynamic.DynamicModuleNetworkFilter{}
			extensionType = dynamicModuleExtensionType
		default:
			return nil, extensionType, nil
		}
		err = typedConfig.UnmarshalTo(filter)
	}
	if err != nil {
		return nil, extensionType, fmt.Errorf("failed to unmarshal extension config: %w", err)
	}
	if filter == nil {
		return nil, extensionType, nil
	}
	parsed := &extensionConfig{config: ec, filter: filter}
	// Local modules already contain the final plugin config and need no agent conversion.
	var code *core.AsyncDataSource
	switch cfg := filter.(type) {
	case *httpwasm.Wasm:
		code = cfg.GetConfig().GetVmConfig().GetCode()
	case *networkwasm.Wasm:
		code = cfg.GetConfig().GetVmConfig().GetCode()
	case *httpdynamic.DynamicModuleFilter:
		code = cfg.GetDynamicModuleConfig().GetModule()
	case *networkdynamic.DynamicModuleNetworkFilter:
		code = cfg.GetDynamicModuleConfig().GetModule()
	}
	if code.GetRemote() == nil {
		if code.GetLocal() == nil {
			return nil, extensionType, fmt.Errorf("no remote and local load found in %T", filter)
		}
		return nil, extensionType, nil
	}
	return parsed, extensionType, nil
}

// unmarshalWasmTypedStruct supports Wasm filters encoded as an outer TypedStruct.
func unmarshalWasmTypedStruct(config *anypb.Any) (proto.Message, error) {
	typedStruct := &udpa.TypedStruct{}
	if err := config.UnmarshalTo(typedStruct); err != nil {
		return nil, fmt.Errorf("failed to unmarshal Wasm typed struct: %w", err)
	}
	var filter proto.Message
	switch typedStruct.TypeUrl {
	case model.WasmHTTPFilterType:
		filter = &httpwasm.Wasm{}
	case model.WasmNetworkFilterType:
		filter = &networkwasm.Wasm{}
	default:
		return nil, nil
	}
	if err := protomarshal.StructToMessageSlow(typedStruct.Value, filter); err != nil {
		return filter, fmt.Errorf("failed to decode Wasm typed struct: %w", err)
	}
	return filter, nil
}

func convertHTTPWasmConfigFromRemoteToLocal(ec *core.TypedExtensionConfig, wasmHTTPFilterConfig *httpwasm.Wasm, cache Cache) (*anypb.Any, error) {
	status := conversionSuccess
	defer func() {
		trafficExtensionConfigConversionCount.
			With(extensionTypeTag.Value(wasmExtensionType), resultTag.Value(status)).
			Increment()
	}()

	// ec.Name is resourceName.
	// https://github.com/istio/istio/blob/9ea7ad532a9cc58a3564143d41ac89a61aaa8058/pilot/pkg/networking/core/v1alpha3/extension/wasmplugin.go#L103
	err := rewriteVMConfig(ec.Name, wasmHTTPFilterConfig.Config.GetVmConfig(), &status, cache, wasmHTTPFilterConfig.Config.Name)
	if err != nil {
		return nil, err
	}

	wasmTypedConfig, err := anypb.New(wasmHTTPFilterConfig)
	if err != nil {
		status = marshalFailure
		return nil, fmt.Errorf("failed to marshal new wasm HTTP filter %+v to protobuf Any: %w", wasmHTTPFilterConfig, err)
	}
	ec.TypedConfig = wasmTypedConfig
	trafficextensionLog.Debugf("new extension config resource %+v", ec)

	nec, err := anypb.New(ec)
	if err != nil {
		status = marshalFailure
		return nil, fmt.Errorf("failed to marshal new extension config resource: %w", err)
	}

	// At this point, we are certain that wasm module has been downloaded and config is rewritten.
	// ECDS will be rewritten successfully.
	return nec, nil
}

func convertNetworkWasmConfigFromRemoteToLocal(ec *core.TypedExtensionConfig, wasmNetworkFilterConfig *networkwasm.Wasm, cache Cache) (*anypb.Any, error) {
	status := conversionSuccess
	defer func() {
		trafficExtensionConfigConversionCount.
			With(extensionTypeTag.Value(wasmExtensionType), resultTag.Value(status)).
			Increment()
	}()

	// ec.Name is resourceName.
	// https://github.com/istio/istio/blob/9ea7ad532a9cc58a3564143d41ac89a61aaa8058/pilot/pkg/networking/core/v1alpha3/extension/wasmplugin.go#L103
	err := rewriteVMConfig(ec.Name, wasmNetworkFilterConfig.Config.GetVmConfig(), &status, cache, wasmNetworkFilterConfig.Config.Name)
	if err != nil {
		return nil, err
	}
	wasmTypedConfig, err := anypb.New(wasmNetworkFilterConfig)
	if err != nil {
		status = marshalFailure
		return nil, fmt.Errorf("failed to marshal new wasm Network filter %+v to protobuf Any: %w", wasmNetworkFilterConfig, err)
	}
	ec.TypedConfig = wasmTypedConfig
	trafficextensionLog.Debugf("new extension config resource %+v", ec)

	nec, err := anypb.New(ec)
	if err != nil {
		status = marshalFailure
		return nil, fmt.Errorf("failed to marshal new extension config resource: %w", err)
	}

	// At this point, we are certain that wasm module has been downloaded and config is rewritten.
	// ECDS will be rewritten successfully.
	return nec, nil
}

func rewriteVMConfig(resourceName string, vm *wasmextensions.VmConfig, status *string, cache Cache, configName string) error {
	envs := vm.GetEnvironmentVariables()
	var pullSecret []byte
	pullPolicy := Unspecified
	resourceVersion := ""
	if envs != nil {
		if sec, found := envs.KeyValues[model.WasmSecretEnv]; found {
			if sec == "" {
				*status = fetchFailure
				return fmt.Errorf("cannot fetch Wasm module %v: missing image pulling secret", configName)
			}
			pullSecret = []byte(sec)
		}

		if ps, found := envs.KeyValues[model.WasmPolicyEnv]; found {
			if p, found := PullPolicyValues[ps]; found {
				pullPolicy = p
			}
		}
		resourceVersion = envs.KeyValues[model.WasmResourceVersionEnv]

		// Strip all internal env variables(with ISTIO_META) from VM env variable.
		// These env variables are added by Istio control plane and meant to be consumed by the
		// agent for image pulling control should not be leaked to Envoy or the Wasm extension runtime.
		for k := range envs.KeyValues {
			if strings.HasPrefix(k, bootstrap.IstioMetaPrefix) {
				delete(envs.KeyValues, k)
			}
		}
		if len(envs.KeyValues) == 0 {
			if len(envs.HostEnvKeys) == 0 {
				vm.EnvironmentVariables = nil
			} else {
				envs.KeyValues = nil
			}
		}
	}
	remote := vm.GetCode().GetRemote()
	httpURI := remote.GetHttpUri()
	if httpURI == nil {
		*status = missRemoteFetchHint
		return fmt.Errorf("wasm remote fetch %+v does not have httpUri specified for config %s", remote, configName)
	}
	// checksum sent by istiod can be "nil" if not set by user - magic value used to avoid unmarshaling errors
	if remote.Sha256 == "nil" {
		remote.Sha256 = ""
	}

	// Default timeout, without this, if a user does not specify a timeout in the config, it fails with deadline exceeded
	// while building transport in go container.
	timeout := time.Second * 5
	if remote.GetHttpUri().Timeout != nil {
		// This is always 30s, because the timeout is set by the control plane when converted to WasmPluginWrapper.
		// see buildDataSource() in pilot/pkg/model/extensions.go
		timeout = remote.GetHttpUri().Timeout.AsDuration()
	}
	f, err := cache.Get(httpURI.GetUri(), GetOptions{
		Checksum:        remote.Sha256,
		ResourceName:    resourceName,
		ResourceVersion: resourceVersion,
		RequestTimeout:  timeout,
		PullSecret:      pullSecret,
		PullPolicy:      pullPolicy,
	})
	if err != nil {
		*status = fetchFailure
		return fmt.Errorf("cannot fetch Wasm module %v: %w", remote.GetHttpUri().GetUri(), err)
	}

	// Rewrite remote fetch to local file.
	vm.Code = &core.AsyncDataSource{
		Specifier: &core.AsyncDataSource_Local{
			Local: &core.DataSource{
				Specifier: &core.DataSource_Filename{
					Filename: f,
				},
			},
		},
	}
	return nil
}
