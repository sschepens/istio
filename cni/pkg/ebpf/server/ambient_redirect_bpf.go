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

// Code generated by bpf2go; DO NOT EDIT.

package server

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"

	"github.com/cilium/ebpf"
)

type ambient_redirectAppInfo struct {
	Ifindex uint32
	MacAddr [6]uint8
	Pads    [2]uint8
}

type ambient_redirectHostInfo struct{ Addr [4]uint32 }

type ambient_redirectZtunnelInfo struct {
	Ifindex uint32
	MacAddr [6]uint8
	Flag    uint8
	Pad     uint8
}

// loadAmbient_redirect returns the embedded CollectionSpec for ambient_redirect.
func loadAmbient_redirect() (*ebpf.CollectionSpec, error) {
	reader := bytes.NewReader(_Ambient_redirectBytes)
	spec, err := ebpf.LoadCollectionSpecFromReader(reader)
	if err != nil {
		return nil, fmt.Errorf("can't load ambient_redirect: %w", err)
	}

	return spec, err
}

// loadAmbient_redirectObjects loads ambient_redirect and converts it into a struct.
//
// The following types are suitable as obj argument:
//
//	*ambient_redirectObjects
//	*ambient_redirectPrograms
//	*ambient_redirectMaps
//
// See ebpf.CollectionSpec.LoadAndAssign documentation for details.
func loadAmbient_redirectObjects(obj interface{}, opts *ebpf.CollectionOptions) error {
	spec, err := loadAmbient_redirect()
	if err != nil {
		return err
	}

	return spec.LoadAndAssign(obj, opts)
}

// ambient_redirectSpecs contains maps and programs before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type ambient_redirectSpecs struct {
	ambient_redirectProgramSpecs
	ambient_redirectMapSpecs
}

// ambient_redirectSpecs contains programs before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type ambient_redirectProgramSpecs struct {
	AppInbound         *ebpf.ProgramSpec `ebpf:"app_inbound"`
	AppOutbound        *ebpf.ProgramSpec `ebpf:"app_outbound"`
	ZtunnelHostIngress *ebpf.ProgramSpec `ebpf:"ztunnel_host_ingress"`
	ZtunnelIngress     *ebpf.ProgramSpec `ebpf:"ztunnel_ingress"`
	ZtunnelTproxy      *ebpf.ProgramSpec `ebpf:"ztunnel_tproxy"`
}

// ambient_redirectMapSpecs contains maps before they are loaded into the kernel.
//
// It can be passed ebpf.CollectionSpec.Assign.
type ambient_redirectMapSpecs struct {
	AppInfo     *ebpf.MapSpec `ebpf:"app_info"`
	HostIpInfo  *ebpf.MapSpec `ebpf:"host_ip_info"`
	LogLevel    *ebpf.MapSpec `ebpf:"log_level"`
	ZtunnelInfo *ebpf.MapSpec `ebpf:"ztunnel_info"`
}

// ambient_redirectObjects contains all objects after they have been loaded into the kernel.
//
// It can be passed to loadAmbient_redirectObjects or ebpf.CollectionSpec.LoadAndAssign.
type ambient_redirectObjects struct {
	ambient_redirectPrograms
	ambient_redirectMaps
}

func (o *ambient_redirectObjects) Close() error {
	return _Ambient_redirectClose(
		&o.ambient_redirectPrograms,
		&o.ambient_redirectMaps,
	)
}

// ambient_redirectMaps contains all maps after they have been loaded into the kernel.
//
// It can be passed to loadAmbient_redirectObjects or ebpf.CollectionSpec.LoadAndAssign.
type ambient_redirectMaps struct {
	AppInfo     *ebpf.Map `ebpf:"app_info"`
	HostIpInfo  *ebpf.Map `ebpf:"host_ip_info"`
	LogLevel    *ebpf.Map `ebpf:"log_level"`
	ZtunnelInfo *ebpf.Map `ebpf:"ztunnel_info"`
}

func (m *ambient_redirectMaps) Close() error {
	return _Ambient_redirectClose(
		m.AppInfo,
		m.HostIpInfo,
		m.LogLevel,
		m.ZtunnelInfo,
	)
}

// ambient_redirectPrograms contains all programs after they have been loaded into the kernel.
//
// It can be passed to loadAmbient_redirectObjects or ebpf.CollectionSpec.LoadAndAssign.
type ambient_redirectPrograms struct {
	AppInbound         *ebpf.Program `ebpf:"app_inbound"`
	AppOutbound        *ebpf.Program `ebpf:"app_outbound"`
	ZtunnelHostIngress *ebpf.Program `ebpf:"ztunnel_host_ingress"`
	ZtunnelIngress     *ebpf.Program `ebpf:"ztunnel_ingress"`
	ZtunnelTproxy      *ebpf.Program `ebpf:"ztunnel_tproxy"`
}

func (p *ambient_redirectPrograms) Close() error {
	return _Ambient_redirectClose(
		p.AppInbound,
		p.AppOutbound,
		p.ZtunnelHostIngress,
		p.ZtunnelIngress,
		p.ZtunnelTproxy,
	)
}

func _Ambient_redirectClose(closers ...io.Closer) error {
	for _, closer := range closers {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	return nil
}

// Do not access this directly.
//
//go:embed ambient_redirect_bpf.o
var _Ambient_redirectBytes []byte
