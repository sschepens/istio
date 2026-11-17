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
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	udpa "github.com/cncf/xds/go/udpa/type/v1"
	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	httpdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/dynamic_modules/v3"
	networkdynamic "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/network/dynamic_modules/v3"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/structpb"

	"istio.io/istio/pkg/model"
)

func TestDynamicModulePlatform(t *testing.T) {
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")
	image := func(arch string) v1.Image {
		var archive bytes.Buffer
		tw := tar.NewWriter(&archive)
		if err := tw.WriteHeader(&tar.Header{Name: "plugin.so", Size: int64(len(arch)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(arch)); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		layer, err := tarball.LayerFromReader(&archive)
		if err != nil {
			t.Fatal(err)
		}
		img, err := mutate.AppendLayers(empty.Image, layer)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := img.ConfigFile()
		if err != nil {
			t.Fatal(err)
		}
		cfg.OS, cfg.Architecture = "linux", arch
		img, err = mutate.ConfigFile(img, cfg)
		if err != nil {
			t.Fatal(err)
		}
		return img
	}
	amd64, arm64 := image("amd64"), image("arm64")
	index := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: amd64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: arm64, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}})
	ref, err := name.ParseReference(host + "/modules:multi")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, index); err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"amd64", "arm64", "s390x"} {
		t.Run(arch, func(t *testing.T) {
			img, err := nativeImage(desc, arch)
			if arch == "s390x" {
				if err == nil {
					t.Fatal("accepted missing architecture")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := extractDynamicModule(img)
			if err != nil || string(b) != arch {
				t.Fatalf("got %q, %v", b, err)
			}
		})
	}
	single, _ := name.ParseReference(host + "/modules:single")
	if err := remote.Write(single, amd64); err != nil {
		t.Fatal(err)
	}
	singleDesc, err := remote.Get(single)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nativeImage(singleDesc, "arm64"); err == nil {
		t.Fatal("accepted wrong single-image architecture")
	}
	missing, _ := name.ParseReference(host + "/modules:missing")
	if err := remote.Write(missing, empty.Image); err != nil {
		t.Fatal(err)
	}
	missingDesc, err := remote.Get(missing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nativeImage(missingDesc, "amd64"); err == nil {
		t.Fatal("accepted missing platform metadata")
	}
	// The public fetcher must select the host architecture and preserve the index digest.
	fetcher := &ImageFetcher{dynamicModule: true}
	fetch, digest, err := fetcher.PrepareFetch(ref.Name())
	if runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64" {
		if err != nil {
			t.Fatal(err)
		}
		expected, _ := index.Digest()
		b, err := fetch()
		if err != nil || string(b) != runtime.GOARCH || digest != expected.Hex {
			t.Fatalf("got %q %s %v", b, digest, err)
		}
	}
}

type dynamicModuleCache struct {
	opts GetOptions
	url  string
	err  error
}

func (c *dynamicModuleCache) Get(url string, opts GetOptions) (string, error) {
	c.url, c.opts = url, opts
	return "/cache/module.so", c.err
}
func (*dynamicModuleCache) Cleanup() {}

func TestDynamicModuleConversion(t *testing.T) {
	for _, typ := range []string{"HTTP", "NETWORK"} {
		t.Run(typ, func(t *testing.T) {
			value, _ := structpb.NewStruct(map[string]any{"url": "registry/module:v1", "pluginName": "test", "type": typ,
				"imagePullSecret": "secret", "imagePullPolicy": "Always", "resourceVersion": "42", "pluginConfig": map[string]any{"key": "value"}})
			envelope, _ := anypb.New(&udpa.TypedStruct{TypeUrl: model.DynamicModuleTransportType, Value: value})
			resource, _ := anypb.New(&core.TypedExtensionConfig{Name: "test", TypedConfig: envelope})
			cache := &dynamicModuleCache{}
			resources := []*anypb.Any{resource}
			if err := MaybeConvertExtensionConfig(resources, cache); err != nil {
				t.Fatal(err)
			}
			if cache.url != "oci://registry/module:v1" || !cache.opts.DynamicModule || cache.opts.ResourceVersion != "42" || cache.opts.PullPolicy != Always || string(cache.opts.PullSecret) != "secret" {
				t.Fatalf("bad options: %+v", cache)
			}
			ec := &core.TypedExtensionConfig{}
			if err := resources[0].UnmarshalTo(ec); err != nil {
				t.Fatal(err)
			}
			if typ == "HTTP" {
				f := &httpdynamic.DynamicModuleFilter{}
				if err := ec.TypedConfig.UnmarshalTo(f); err != nil {
					t.Fatal(err)
				}
				if f.GetDynamicModuleConfig().GetModule().GetLocal().GetFilename() != "/cache/module.so" || f.FilterName != "test" {
					t.Fatal(f)
				}
			} else {
				f := &networkdynamic.DynamicModuleNetworkFilter{}
				if err := ec.TypedConfig.UnmarshalTo(f); err != nil {
					t.Fatal(err)
				}
				if f.GetDynamicModuleConfig().GetModule().GetLocal().GetFilename() != "/cache/module.so" {
					t.Fatal(f)
				}
			}
			cache.err = errors.New("no matching architecture")
			resources = []*anypb.Any{resource}
			if err := MaybeConvertExtensionConfig(resources, cache); err == nil || !strings.Contains(err.Error(), "no matching architecture") {
				t.Fatalf("expected NACK: %v", err)
			}
		})
	}
}

func TestDynamicModuleCacheIsolation(t *testing.T) {
	wasm := cacheKey{moduleKey: moduleKey{name: "oci://test/module", checksum: "abc"}, downloadURL: "oci://test/module:v1"}
	dynamic := wasm
	dynamic.dynamicModule = true
	if wasm.moduleKey == dynamic.moduleKey || wasm.checksumURL() == dynamic.checksumURL() {
		t.Fatal("cache keys collide")
	}
	path, err := getModulePath(t.TempDir(), dynamic.moduleKey)
	if err != nil || !strings.HasSuffix(path, ".so") {
		t.Fatal(fmt.Sprint(path, err))
	}
}

func TestDynamicModuleLocalAndMissingSecret(t *testing.T) {
	for _, tc := range []struct {
		url     string
		secret  bool
		wantErr bool
	}{
		{"file:///opt/filters/module.so", false, false},
		{"oci://registry/module:v1", true, true},
	} {
		value, _ := structpb.NewStruct(map[string]any{"url": tc.url})
		if tc.secret {
			value.Fields["imagePullSecret"] = structpb.NewStringValue("")
		}
		envelope, _ := anypb.New(&udpa.TypedStruct{TypeUrl: model.DynamicModuleTransportType, Value: value})
		resource, _ := anypb.New(&core.TypedExtensionConfig{Name: "test", TypedConfig: envelope})
		cache := &dynamicModuleCache{}
		converted, handled, err := convertDynamicModule(resource, cache)
		if !handled || (err != nil) != tc.wantErr {
			t.Fatalf("handled=%v err=%v", handled, err)
		}
		if cache.url != "" {
			t.Fatal("unexpected fetch")
		}
		if err == nil {
			ec := &core.TypedExtensionConfig{}
			if err := converted.UnmarshalTo(ec); err != nil {
				t.Fatal(err)
			}
			f := &httpdynamic.DynamicModuleFilter{}
			if err := ec.TypedConfig.UnmarshalTo(f); err != nil {
				t.Fatal(err)
			}
			if f.GetDynamicModuleConfig().GetModule().GetLocal().GetFilename() != "/opt/filters/module.so" {
				t.Fatal(f)
			}
		}
	}
}

func TestDynamicModuleHTTPCache(t *testing.T) {
	machine, ok := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[runtime.GOARCH]
	if !ok {
		t.Skip("ELF test fixture supports amd64 and arm64")
	}
	b := make([]byte, 64)
	copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(b[16:], uint16(elf.ET_DYN))
	binary.LittleEndian.PutUint16(b[18:], uint16(machine))
	binary.LittleEndian.PutUint32(b[20:], 1)
	binary.LittleEndian.PutUint16(b[52:], 64)
	if !isValidDynamicModule(b) {
		t.Fatal("invalid ELF fixture")
	}
	digest := sha256.Sum256(b)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(b) }))
	defer server.Close()
	cache := NewLocalFileCache(t.TempDir(), defaultOptions())
	defer cache.Cleanup()
	opts := GetOptions{DynamicModule: true, Checksum: hex.EncodeToString(digest[:]), RequestTimeout: time.Second}
	path, err := cache.Get(server.URL+"/module.so", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, ".so") {
		t.Fatal(path)
	}
	stored, err := os.ReadFile(path)
	if err != nil || string(stored) != string(b) {
		t.Fatal("incorrect library cached", err)
	}
	// A cached native library must not satisfy a Wasm request for the same URL and digest.
	opts.DynamicModule = false
	if _, err := cache.Get(server.URL+"/module.so", opts); err == nil {
		t.Fatal("native library used as Wasm")
	}
	opts.DynamicModule = true
	opts.Checksum = strings.Repeat("0", 64)
	if _, err := cache.Get(server.URL+"/module.so", opts); err == nil {
		t.Fatal("accepted checksum mismatch")
	}
	other := elf.EM_X86_64
	if machine == other {
		other = elf.EM_AARCH64
	}
	binary.LittleEndian.PutUint16(b[18:], uint16(other))
	if isValidDynamicModule(b) {
		t.Fatal("accepted foreign architecture ELF")
	}
}
