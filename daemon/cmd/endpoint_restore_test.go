// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package cmd

import (
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/cilium/hive/hivetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"github.com/cilium/cilium/pkg/datapath/linux/safenetlink"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/testutils"
	"github.com/cilium/cilium/pkg/testutils/netns"
)

func TestPrivilegedRemoveStaleEPIfaces(t *testing.T) {
	testutils.PrivilegedTest(t)

	ns := netns.NewNetNS(t)

	ns.Do(func() error {
		linkAttrs := netlink.NewLinkAttrs()
		linkAttrs.Name = "lxc12345"
		veth := &netlink.Veth{
			LinkAttrs: linkAttrs,
			PeerName:  "tmp54321",
		}

		err := netlink.LinkAdd(veth)
		assert.NoError(t, err)

		_, err = safenetlink.LinkByName(linkAttrs.Name)
		assert.NoError(t, err)

		restorer := &endpointRestorer{logger: hivetest.Logger(t)}
		err = restorer.clearStaleCiliumEndpointVeths()
		assert.NoError(t, err)

		// Check that stale iface is removed
		_, err = safenetlink.LinkByName(linkAttrs.Name)
		assert.Error(t, err)

		return nil
	})
}

func withRestoreScaleToZeroFake(t *testing.T, enabled bool, err error) *[]net.IP {
	t.Helper()

	prevEnabled := option.Config.EnableScaleToZeroDatapath
	prevFn := restoreScaleToZeroFn
	t.Cleanup(func() {
		option.Config.EnableScaleToZeroDatapath = prevEnabled
		restoreScaleToZeroFn = prevFn
	})
	option.Config.EnableScaleToZeroDatapath = enabled

	calls := &[]net.IP{}
	restoreScaleToZeroFn = func(ip net.IP) error {
		*calls = append(*calls, ip)
		return err
	}
	return calls
}

func TestClearRestoredEndpointScaleToZeroState_DisabledIsNoop(t *testing.T) {
	calls := withRestoreScaleToZeroFake(t, false, nil)

	clearRestoredEndpointScaleToZeroState(hivetest.Logger(t), 42, netip.MustParseAddr("10.0.0.1"))

	assert.Empty(t, *calls)
}

func TestClearRestoredEndpointScaleToZeroState_InvalidAddrIsNoop(t *testing.T) {
	calls := withRestoreScaleToZeroFake(t, true, nil)

	clearRestoredEndpointScaleToZeroState(hivetest.Logger(t), 42, netip.Addr{})

	assert.Empty(t, *calls)
}

func TestClearRestoredEndpointScaleToZeroState_EnabledInvokesCleanup(t *testing.T) {
	calls := withRestoreScaleToZeroFake(t, true, nil)
	addr := netip.MustParseAddr("10.0.0.1")

	clearRestoredEndpointScaleToZeroState(hivetest.Logger(t), 42, addr)

	require.Len(t, *calls, 1)
	assert.True(t, (*calls)[0].Equal(net.IP(addr.AsSlice())))
}

func TestClearRestoredEndpointScaleToZeroState_CleanupErrorIsNotFatal(t *testing.T) {
	withRestoreScaleToZeroFake(t, true, errors.New("boom"))

	assert.NotPanics(t, func() {
		clearRestoredEndpointScaleToZeroState(hivetest.Logger(t), 42, netip.MustParseAddr("10.0.0.1"))
	})
}
