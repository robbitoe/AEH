package egress

import "net/netip"

// nonPublic lists ranges the proxy never connects to besides those the netip predicates
// cover: shared address space, IETF protocol assignments, benchmarking, documentation,
// reserved and broadcast, NAT64 (which embeds an IPv4 address of any kind), 6to4 and
// Teredo (same), and IPv6 documentation.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("100::/64"),
}

// Public reports an address the proxy may connect to: not loopback, private, link-local
// (which holds cloud metadata services), multicast, unspecified or any range above. An
// IPv4-mapped IPv6 address is judged as its IPv4 address.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.Zone() != "" || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return a.IsGlobalUnicast()
}
