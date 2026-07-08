// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

// Package constellationstz provides Go-side access to the pinned BPF maps
// backing the scale-to-zero (STZ) datapath (bpf/lib/constellation_stz.h).
//
// The maps are created and pinned by the datapath itself, not by this
// package, so nothing here ever creates, resizes, or recreates them — it
// only opens the existing pins to delete keys.
package constellationstz

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/cilium/cilium/pkg/bpf"
	"github.com/cilium/cilium/pkg/ebpf"
	"github.com/cilium/cilium/pkg/types"
)

const (
	triggersMapName = "constellation_stz_triggers"
	flowsMapName    = "constellation_stz_flows"

	triggersMaxEntries = 1024
	flowsMaxEntries    = 4096
)

// Key implements bpf.MapKey. Must stay in sync with the __be32 key used by
// constellation_stz_triggers / constellation_stz_flows in
// bpf/lib/constellation_stz.h.
type Key struct {
	Addr types.IPv4
}

func (k *Key) String() string  { return k.Addr.String() }
func (k *Key) New() bpf.MapKey { return &Key{} }

func newKey(ip net.IP) (*Key, bool) {
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, false
	}
	k := &Key{}
	copy(k.Addr[:], ip4)
	return k, true
}

// triggerValue implements bpf.MapValue for constellation_stz_triggers (__u8).
type triggerValue struct {
	Armed uint8
}

func (v *triggerValue) String() string    { return fmt.Sprintf("%d", v.Armed) }
func (v *triggerValue) New() bpf.MapValue { return &triggerValue{} }

// flowValue implements bpf.MapValue for constellation_stz_flows (__u64).
type flowValue struct {
	LastSeen uint64
}

func (v *flowValue) String() string    { return fmt.Sprintf("%d", v.LastSeen) }
func (v *flowValue) New() bpf.MapValue { return &flowValue{} }

var (
	triggersMap     *bpf.Map
	triggersMapOnce sync.Once
	flowsMap        *bpf.Map
	flowsMapOnce    sync.Once
)

func getTriggersMap() *bpf.Map {
	triggersMapOnce.Do(func() {
		triggersMap = bpf.NewMap(
			triggersMapName,
			ebpf.Hash,
			&Key{},
			&triggerValue{},
			triggersMaxEntries,
			unix.BPF_F_NO_PREALLOC,
		)
	})
	return triggersMap
}

func getFlowsMap() *bpf.Map {
	flowsMapOnce.Do(func() {
		flowsMap = bpf.NewMap(
			flowsMapName,
			ebpf.Hash,
			&Key{},
			&flowValue{},
			flowsMaxEntries,
			unix.BPF_F_NO_PREALLOC,
		)
	})
	return flowsMap
}

// Clear removes any trigger/flow-tracking state for ip from the scale-to-zero
// datapath maps.
//
// This is meant to be called whenever IPAM allocates or releases ip, so that
// an armed trigger left behind by a crashed or restarted controller (which
// would otherwise survive independently of any pod and silently black-hole
// every future SYN sent to ip, including to an unrelated pod that is later
// assigned the same address) can never outlive the IP it was armed for.
//
// A missing key is not an error. Callers should treat a non-nil error as
// advisory (log it) rather than fatal — a lookup/delete failure here must
// never block IP allocation or release.
func Clear(ip net.IP) error {
	key, ok := newKey(ip)
	if !ok {
		// Not an IPv4 address: the STZ datapath only tracks IPv4 (ingress4).
		return nil
	}

	var errs []error
	if _, err := getTriggersMap().SilentDelete(key); err != nil {
		errs = append(errs, fmt.Errorf("clearing %s: %w", triggersMapName, err))
	}
	if _, err := getFlowsMap().SilentDelete(key); err != nil {
		errs = append(errs, fmt.Errorf("clearing %s: %w", flowsMapName, err))
	}
	return errors.Join(errs...)
}
