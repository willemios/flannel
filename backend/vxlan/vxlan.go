// Copyright 2015 CoreOS, Inc.
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

package vxlan

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/coreos/flannel/Godeps/_workspace/src/github.com/golang/glog"
	"github.com/coreos/flannel/Godeps/_workspace/src/golang.org/x/net/context"

	"github.com/coreos/flannel/backend"
	"github.com/coreos/flannel/pkg/ip"
	"github.com/coreos/flannel/subnet"
)

const (
	defaultVNI = 1
)

type VXLANBackend struct {
	sm      subnet.Manager
	network string
	config  *subnet.Config
	cfg     struct {
		VNI  int
		Port int
	}
	lease  *subnet.Lease
	dev    *vxlanDevice
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(sm subnet.Manager, network string, config *subnet.Config) backend.Backend {
	ctx, cancel := context.WithCancel(context.Background())

	vb := &VXLANBackend{
		sm:      sm,
		network: network,
		config:  config,
		ctx:     ctx,
		cancel:  cancel,
	}
	vb.cfg.VNI = defaultVNI

	return vb
}

func newSubnetAttrs(pubIP net.IP, mac net.HardwareAddr) (*subnet.LeaseAttrs, error) {
	data, err := json.Marshal(&vxlanLeaseAttrs{hardwareAddr(mac)})
	if err != nil {
		return nil, err
	}

	return &subnet.LeaseAttrs{
		PublicIP:    ip.FromIP(pubIP),
		BackendType: "vxlan",
		BackendData: json.RawMessage(data),
	}, nil
}

func (vb *VXLANBackend) Init(extIface *net.Interface, extIP net.IP) (*backend.SubnetDef, error) {
	// Parse our configuration
	if len(vb.config.Backend) > 0 {
		if err := json.Unmarshal(vb.config.Backend, &vb.cfg); err != nil {
			return nil, fmt.Errorf("error decoding VXLAN backend config: %v", err)
		}
	}

	devAttrs := vxlanDeviceAttrs{
		vni:       uint32(vb.cfg.VNI),
		name:      fmt.Sprintf("flannel.%v", vb.cfg.VNI),
		vtepIndex: extIface.Index,
		vtepAddr:  extIP,
		vtepPort:  vb.cfg.Port,
	}

	var err error
	for {
		vb.dev, err = newVXLANDevice(&devAttrs)
		if err == nil {
			break
		} else {
			log.Error("VXLAN init: ", err)
			log.Info("Retrying in 1 second...")

			// wait 1 sec before retrying
			time.Sleep(1 * time.Second)
		}
	}

	sa, err := newSubnetAttrs(extIP, vb.dev.MACAddr())
	if err != nil {
		return nil, err
	}

	l, err := vb.sm.AcquireLease(vb.ctx, vb.network, sa)
	switch err {
	case nil:
		vb.lease = l

	case context.Canceled, context.DeadlineExceeded:
		return nil, err

	default:
		return nil, fmt.Errorf("failed to acquire lease: %v", err)
	}

	// vxlan's subnet is that of the whole overlay network (e.g. /16)
	// and not that of the individual host (e.g. /24)
	vxlanNet := ip.IP4Net{
		IP:        l.Subnet.IP,
		PrefixLen: vb.config.Network.PrefixLen,
	}
	if err = vb.dev.Configure(vxlanNet); err != nil {
		return nil, err
	}

	return &backend.SubnetDef{
		Net: l.Subnet,
		MTU: vb.dev.MTU(),
	}, nil
}

func (vb *VXLANBackend) Run() {
	vb.wg.Add(1)
	go func() {
		subnet.LeaseRenewer(vb.ctx, vb.sm, vb.network, vb.lease)
		log.Info("LeaseRenewer exited")
		vb.wg.Done()
	}()

	log.Info("Watching for new subnet leases")
	evts := make(chan []subnet.Event)
	vb.wg.Add(1)
	go func() {
		subnet.WatchLeases(vb.ctx, vb.sm, vb.network, evts)
		log.Info("WatchLeases exited")
		vb.wg.Done()
	}()

	defer vb.wg.Wait()

	for {
		select {
		case evtBatch := <-evts:
			vb.handleSubnetEvents(evtBatch)

		case <-vb.ctx.Done():
			return
		}
	}
}

func (vb *VXLANBackend) Stop() {
	vb.cancel()
}

func (vb *VXLANBackend) Name() string {
	return "VXLAN"
}

// So we can make it JSON (un)marshalable
type hardwareAddr net.HardwareAddr

func (hw hardwareAddr) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", net.HardwareAddr(hw))), nil
}

func (hw *hardwareAddr) UnmarshalJSON(b []byte) error {
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return fmt.Errorf("error parsing hardware addr")
	}

	b = b[1 : len(b)-1]

	mac, err := net.ParseMAC(string(b))
	if err != nil {
		return err
	}

	*hw = hardwareAddr(mac)
	return nil
}

type vxlanLeaseAttrs struct {
	VtepMAC hardwareAddr
}

func (vb *VXLANBackend) handleSubnetEvents(batch []subnet.Event) {
	for _, evt := range batch {
		switch evt.Type {
		case subnet.SubnetAdded:
			log.Info("Subnet added: ", evt.Lease.Subnet)

			if evt.Lease.Attrs.BackendType != "vxlan" {
				log.Warningf("Ignoring non-vxlan subnet: type=%v", evt.Lease.Attrs.BackendType)
				continue
			}

			var attrs vxlanLeaseAttrs
			if err := json.Unmarshal(evt.Lease.Attrs.BackendData, &attrs); err != nil {
				log.Error("Error decoding subnet lease JSON: ", err)
				continue
			}
			vb.dev.AddL2(neigh{IP: evt.Lease.Attrs.PublicIP, MAC: net.HardwareAddr(attrs.VtepMAC)})
			vb.dev.AddL3(neigh{IP: evt.Lease.Subnet.IP, MAC: net.HardwareAddr(attrs.VtepMAC)})
			vb.dev.AddRoute(evt.Lease.Subnet)

		case subnet.SubnetRemoved:
			log.Info("Subnet removed: ", evt.Lease.Subnet)

			if evt.Lease.Attrs.BackendType != "vxlan" {
				log.Warningf("Ignoring non-vxlan subnet: type=%v", evt.Lease.Attrs.BackendType)
				continue
			}

			var attrs vxlanLeaseAttrs
			if err := json.Unmarshal(evt.Lease.Attrs.BackendData, &attrs); err != nil {
				log.Error("Error decoding subnet lease JSON: ", err)
				continue
			}

			vb.dev.DelRoute(evt.Lease.Subnet)
			if len(attrs.VtepMAC) > 0 {
				vb.dev.DelL2(neigh{IP: evt.Lease.Attrs.PublicIP, MAC: net.HardwareAddr(attrs.VtepMAC)})
				vb.dev.DelL3(neigh{IP: evt.Lease.Subnet.IP, MAC: net.HardwareAddr(attrs.VtepMAC)})
			}

		default:
			log.Error("Internal error: unknown event type: ", int(evt.Type))
		}
	}
}
