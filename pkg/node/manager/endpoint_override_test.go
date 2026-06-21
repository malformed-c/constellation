// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package manager

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTunnelEndpointOverrides(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[string]netip.Addr
		wantErr bool
	}{
		{
			name:  "empty",
			input: "",
			want:  nil,
		},
		{
			name:  "single entry",
			input: "engix99=192.168.50.1",
			want:  map[string]netip.Addr{"engix99": netip.MustParseAddr("192.168.50.1")},
		},
		{
			name:  "multiple entries",
			input: "node-a=10.0.0.1,node-b=10.0.0.2",
			want: map[string]netip.Addr{
				"node-a": netip.MustParseAddr("10.0.0.1"),
				"node-b": netip.MustParseAddr("10.0.0.2"),
			},
		},
		{
			name:  "whitespace tolerance",
			input: " engix99 = 192.168.50.1 , engifire = 192.168.50.2 ",
			want: map[string]netip.Addr{
				"engix99":  netip.MustParseAddr("192.168.50.1"),
				"engifire": netip.MustParseAddr("192.168.50.2"),
			},
		},
		{
			name:    "missing ip",
			input:   "engix99=",
			wantErr: true,
		},
		{
			name:    "missing node name",
			input:   "=192.168.50.1",
			wantErr: true,
		},
		{
			name:    "invalid ip",
			input:   "engix99=notanip",
			wantErr: true,
		},
		{
			name:    "no equals sign",
			input:   "engix99",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTunnelEndpointOverrides(tt.input)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func withFakePrefixes(prefixes []netip.Prefix, fn func()) {
	orig := connectedPrefixesFunc
	connectedPrefixesFunc = func() []netip.Prefix { return prefixes }
	defer func() { connectedPrefixesFunc = orig }()
	fn()
}

func TestLookupTunnelEndpointOverride(t *testing.T) {
	overrides := map[string]netip.Addr{
		"engix99": netip.MustParseAddr("192.168.50.1"),
	}
	fallback := netip.MustParseAddr("192.168.100.200")

	// Override applied when locally reachable.
	withFakePrefixes([]netip.Prefix{netip.MustParsePrefix("192.168.50.0/24")}, func() {
		assert.Equal(t, netip.MustParseAddr("192.168.50.1"),
			lookupTunnelEndpointOverride(overrides, "engix99", fallback))
	})

	// Override skipped when NOT locally reachable (e.g. VPS without ethernet).
	withFakePrefixes([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}, func() {
		assert.Equal(t, fallback,
			lookupTunnelEndpointOverride(overrides, "engix99", fallback))
	})

	// No matching node name → fallback regardless of reachability.
	withFakePrefixes([]netip.Prefix{netip.MustParsePrefix("192.168.50.0/24")}, func() {
		assert.Equal(t, fallback,
			lookupTunnelEndpointOverride(overrides, "other-node", fallback))
	})

	// Nil overrides → always fallback.
	withFakePrefixes([]netip.Prefix{netip.MustParsePrefix("192.168.50.0/24")}, func() {
		assert.Equal(t, fallback,
			lookupTunnelEndpointOverride(nil, "engix99", fallback))
	})

	// No local interfaces → override skipped.
	withFakePrefixes(nil, func() {
		assert.Equal(t, fallback,
			lookupTunnelEndpointOverride(overrides, "engix99", fallback))
	})
}
