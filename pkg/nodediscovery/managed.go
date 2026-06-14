// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Constellation

package nodediscovery

// Constellation fork addition: propagate this agent's CiliumInternalIP onto
// the managed pawn CiliumNodes (perigeos host sharding). Kept in its own file
// so nodediscovery.go only differs from upstream by the two call sites that
// invoke updateManagedCiliumInternalIPs.

import (
	"context"
	"fmt"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/net"

	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/node"
	nodeAddressing "github.com/cilium/cilium/pkg/node/addressing"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	"github.com/cilium/cilium/pkg/time"
)

// updateManagedCiliumInternalIPs propagates this agent's CiliumInternalIP
// (and HealthAddressing) onto every managed pawn CiliumNode. Constellation
// runs one cilium-agent per host shared across multiple pawns, so all pawns
// share the host's single cilium_host interface — they should advertise the
// same CiliumInternalIP. Without this, `kubectl get ciliumnode` shows an
// empty CILIUMINTERNALIP column for every pawn and hubble health probes
// have no source address. Other CiliumNode fields (InternalIP set by
// perigeos, IPAM.PodCIDRs set by the operator) are preserved.
func (n *NodeDiscovery) updateManagedCiliumInternalIPs(ctx context.Context, ln *node.LocalNode) {
	managed := nodeTypes.GetManagedNames()
	if len(managed) <= 1 {
		return
	}
	localName := nodeTypes.GetName()

	var v4, v6 string
	if ip := ln.GetCiliumInternalIP(false); ip != nil {
		v4 = ip.String()
	}
	if ip := ln.GetCiliumInternalIP(true); ip != nil {
		v6 = ip.String()
	}
	if v4 == "" && v6 == "" {
		return
	}

	var healthV4, healthV6 string
	if ip := ln.IPv4HealthIP; ip.IsValid() {
		healthV4 = ip.String()
	}
	if ip := ln.IPv6HealthIP; ip.IsValid() {
		healthV6 = ip.String()
	}

	for _, name := range managed {
		if name == localName {
			continue
		}
		if err := n.syncManagedCiliumInternalIP(ctx, name, v4, v6, healthV4, healthV6); err != nil {
			n.logger.Warn("Unable to sync CiliumInternalIP onto managed CiliumNode",
				logfields.NodeName, name,
				logfields.Error, err,
			)
		}
	}
}

func (n *NodeDiscovery) syncManagedCiliumInternalIP(ctx context.Context, name, v4, v6, healthV4, healthV6 string) error {
	for retry := range maxRetryCount {
		cn, err := n.k8sGetters.GetCiliumNode(ctx, name)
		if err != nil {
			return fmt.Errorf("get: %w", err)
		}

		changed := false
		upsert := func(family net.IPFamily, ip string) {
			if ip == "" {
				return
			}
			for i, a := range cn.Spec.Addresses {
				if a.Type != nodeAddressing.NodeCiliumInternalIP {
					continue
				}
				if net.IPFamilyOfString(a.IP) != family {
					continue
				}
				if a.IP != ip {
					cn.Spec.Addresses[i].IP = ip
					changed = true
				}
				return
			}
			cn.Spec.Addresses = append(cn.Spec.Addresses, ciliumv2.NodeAddress{
				Type: nodeAddressing.NodeCiliumInternalIP,
				IP:   ip,
			})
			changed = true
		}
		upsert(net.IPv4, v4)
		upsert(net.IPv6, v6)

		if healthV4 != "" && cn.Spec.HealthAddressing.IPv4 != healthV4 {
			cn.Spec.HealthAddressing.IPv4 = healthV4
			changed = true
		}
		if healthV6 != "" && cn.Spec.HealthAddressing.IPv6 != healthV6 {
			cn.Spec.HealthAddressing.IPv6 = healthV6
			changed = true
		}

		if !changed {
			return nil
		}

		if _, err := n.clientset.CiliumV2().CiliumNodes().Update(ctx, cn, metav1.UpdateOptions{}); err != nil {
			if k8serrors.IsConflict(err) {
				n.logger.Info("Conflict updating managed CiliumNode, retrying",
					logfields.NodeName, name,
					logfields.Retries, retry,
				)
				time.Sleep(backoffDuration)
				continue
			}
			return err
		}
		n.logger.Info("Synced CiliumInternalIP onto managed CiliumNode",
			logfields.NodeName, name,
			logfields.IPAddr, v4,
		)
		return nil
	}
	return fmt.Errorf("conflict retries exhausted")
}
