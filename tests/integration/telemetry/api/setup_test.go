//go:build integ
// +build integ

// Copyright Istio Authors. All Rights Reserved.
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

package api

import (
	"encoding/base64"
	"fmt"
	"testing"

	"istio.io/istio/pkg/test/echo/common"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
	cdeployment "istio.io/istio/pkg/test/framework/components/echo/common/deployment"
	"istio.io/istio/pkg/test/framework/components/echo/match"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/components/istio/ingress"
	"istio.io/istio/pkg/test/framework/components/prometheus"
	"istio.io/istio/pkg/test/framework/label"
	"istio.io/istio/pkg/test/framework/resource"
)

const (
	DefaultBucketCount = 20
)

var (
	apps cdeployment.SingleNamespaceView

	mockProm echo.Instances
	ist      istio.Instance
	promInst prometheus.Instance
	ingr     []ingress.Instance
)

func TestMain(m *testing.M) {
	framework.NewSuite(m).
		Label(label.CustomSetup).
		// TODO: Remove this restriction once we validate our prometheus helm chart version is high enough
		Label(label.IPv4). // https://github.com/istio/istio/issues/35915
		Setup(istio.Setup(&ist, setupConfig)).
		Setup(func(ctx resource.Context) error {
			i, err := istio.Get(ctx)
			if err != nil {
				return err
			}
			return ctx.ConfigIstio().YAML(i.Settings().SystemNamespace, `
apiVersion: telemetry.istio.io/v1alpha1
kind: Telemetry
metadata:
  name: mesh-default
spec:
  metrics:
  - providers:
    - name: prometheus
`).Apply()
		}).
		Setup(testRegistrySetup).
		Setup(SetupSuite).
		Setup(setupWasmExtension).
		Run()
}

func setupConfig(c resource.Context, cfg *istio.Config) {
	if cfg == nil {
		return
	}
	cfg.ControlPlaneValues = `
meshConfig:
  accessLogFile: "" # disable from install, we will enable via Telemetry layer
  defaultConfig:
    proxyStatsMatcher:
      inclusionPrefixes:
      - istio_custom_total
    extraStatTags:
    - url_path
    - response_status
values:
 telemetry:
   v2:
     enabled: false # disable from install, we will enable via Telemetry layer
`
	cfg.RemoteClusterValues = cfg.ControlPlaneValues
}

// SetupSuite set up echo app for stats testing.
func SetupSuite(ctx resource.Context) (err error) {
	echos := (&cdeployment.Config{}).DefaultEchoConfigs(ctx)
	customBuckets := `{"istio":[1,5,10,50,100,500,1000,5000,10000]}`
	proxyMetadata := fmt.Sprintf(`
proxyMetadata:
  WASM_INSECURE_REGISTRIES: %q`, registry.Address())
	for _, e := range echos {
		if e.Subsets[0].Annotations == nil {
			e.Subsets[0].Annotations = map[echo.Annotation]*echo.AnnotationValue{}
		}
		if e.Service == "b" {
			e.Subsets[0].Annotations.Set(echo.SidecarStatsHistogramBuckets, customBuckets)
		}
		e.Subsets[0].Annotations.Set(echo.SidecarProxyConfig, proxyMetadata)
	}
	prom := echo.Config{
		// mock prom instance is used to mock a prometheus server, which will visit other echo instance /metrics
		// endpoint with proxy provisioned certs.
		Service: "mock-prom",
		Subsets: []echo.SubsetConfig{
			{
				Annotations: map[echo.Annotation]*echo.AnnotationValue{
					echo.SidecarIncludeInboundPorts: {
						Value: "",
					},
					echo.SidecarIncludeOutboundIPRanges: {
						Value: "",
					},
					echo.SidecarProxyConfig: {
						Value: `proxyMetadata:
  OUTPUT_CERTS: /etc/certs/custom`,
					},
					echo.SidecarVolumeMount: {
						Value: `[{"name": "custom-certs", "mountPath": "/etc/certs/custom"}]`,
					},
				},
			},
		},
		TLSSettings: &common.TLSSettings{
			ProxyProvision: true,
		},
		Ports: []echo.Port{},
	}
	echos = append(echos, prom)

	if err := cdeployment.SetupSingleNamespace(&apps, cdeployment.Config{Configs: echo.ConfigFuture(&echos)})(ctx); err != nil {
		return err
	}

	if err != nil {
		return err
	}
	for _, c := range ctx.Clusters() {
		ingr = append(ingr, ist.IngressFor(c))
	}
	mockProm = match.ServiceName(echo.NamespacedName{Name: "mock-prom", Namespace: apps.Namespace}).GetMatches(apps.Echos().All.Instances())
	promInst, err = prometheus.New(ctx, prometheus.Config{})
	if err != nil {
		return
	}

	args := map[string]any{
		"DockerConfigJson": base64.StdEncoding.EncodeToString(
			[]byte(createDockerCredential(registryUser, registryPasswd, registry.Address()))),
	}
	if err := ctx.ConfigIstio().EvalFile(apps.Namespace.Name(), args, "testdata/registry-secret.yaml").
		Apply(); err != nil {
		return err
	}
	return nil
}
