/* SPDX-License-Identifier: (GPL-2.0-only OR BSD-2-Clause) */
/* Copyright Authors of Cilium */

#pragma once

#include "common.h"
#include "l4.h"

/* Scale-to-zero (STZ) datapath: SYN trap + flow tracking for idle pod waking.
 *
 * Three BPF maps back the periapsis agent's Datapath interface:
 *   triggers — armed (idle) pod IPs; SYN to these → ringbuf event + drop
 *   flows    — all managed pod IPs; every ingress packet updates last-seen
 *   events   — ringbuf carrying wake events (dest pod IP) to userspace
 *
 * Gated by CONSTELLATION_ENABLE_STZ (set in node_config.h when the agent
 * flag --enable-scale-to-zero-datapath is true).
 */

struct stz_event {
	__be32 addr;
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __be32);
	__type(value, __u8);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
	__uint(max_entries, 1024);
} constellation_stz_triggers __section_maps_btf;

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__type(key, __be32);
	__type(value, __u64);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
	__uint(max_entries, 4096);
} constellation_stz_flows __section_maps_btf;

struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 16384);
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} constellation_stz_events __section_maps_btf;

static __always_inline int
constellation_stz_ingress4(struct __ctx_buff *ctx, int l3_off,
			   const struct iphdr *ip4)
{
	/* Cache all ip4 field reads BEFORE any BPF helper calls —
	 * map_lookup_elem invalidates direct packet data pointers.
	 */
	__be32 daddr = ip4->daddr;
	__u8 protocol = ip4->protocol;
	int l4_off = l3_off + ipv4_hdrlen(ip4);

	__u64 *ts = map_lookup_elem(&constellation_stz_flows, &daddr);
	if (ts)
		*ts = ktime_get_ns();

	if (!map_lookup_elem(&constellation_stz_triggers, &daddr))
		return CTX_ACT_OK;

	if (protocol != IPPROTO_TCP)
		return CTX_ACT_OK;

	union tcp_flags flags = {};

	if (l4_load_tcp_flags(ctx, l4_off, &flags) < 0)
		return CTX_ACT_OK;

	if (!((flags.value & TCP_FLAG_SYN) && !(flags.value & TCP_FLAG_ACK)))
		return CTX_ACT_OK;

	struct stz_event *evt =
		ringbuf_reserve(&constellation_stz_events, sizeof(*evt), 0);
	if (evt) {
		evt->addr = daddr;
		ringbuf_submit(evt, 0);
	}

	return CTX_ACT_DROP;
}
