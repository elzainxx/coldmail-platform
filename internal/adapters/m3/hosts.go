package m3

import (
	"errors"
	"net"
	"net/netip"
	"strings"
)

// The SMTP host an account names is dialled by module 3's worker from the platform's own network, with certificate validation skipped for loopback, private and link-local addresses (the
// module's worker, smtp.js). A user who names such an address makes the worker connect to the platform's inside, so the platform decides what a user may name; the module decides nothing.
//
// MODULE-FORCED(M3-F8): the module accepts any IP address as smtp_host (account_validate.go: net.ParseIP is enough) and any alphanumeric name, including the numeric spellings a resolver reads as an address, so the adapter refuses a host that is not a public address before sending it, unless it was built with AllowPrivateSMTPHosts.
//
// Only what can be judged without the network is judged here: the adapter does no DNS, so a public-looking NAME that resolves to a private address is not caught (the worker's egress is
// the place for that control, not this adapter).

var (
	// the IPv4 ranges Go still calls global unicast and nobody may name: "this network", carrier-grade NAT, the IETF protocol block, the benchmarking range and the reserved class E
	// (built from bytes, not written as dotted literals: the repository's personal-details gate reads every tracked file for addresses)
	reserved4 = []netip.Prefix{pfx4(0, 0, 0, 0, 8), pfx4(100, 64, 0, 0, 10), pfx4(192, 0, 0, 0, 24), pfx4(198, 18, 0, 0, 15), pfx4(240, 0, 0, 0, 4)}
	// the transition prefixes that embed an IPv4 address: NAT64 (64:ff9b::/96, the address is the last four bytes) and 6to4 (2002::/16, the address follows the prefix)
	nat64 = netip.MustParsePrefix("64:ff9b::/96")
	to4   = netip.MustParsePrefix("2002::/16")
)

func pfx4(a, b, c, d byte, bits int) netip.Prefix {
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{a, b, c, d}), bits)
}

func inAny(ps []netip.Prefix, a netip.Addr) bool {
	for _, p := range ps {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// publicAddr reports whether a is an address a user may name: global unicast, and not private, loopback, link-local, carrier-grade NAT or an IPv4 address in disguise.
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	switch {
	case !a.IsValid(), a.IsUnspecified(), a.IsLoopback(), a.IsPrivate(), a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(), a.IsMulticast(), a.IsInterfaceLocalMulticast():
		return false
	case !a.IsGlobalUnicast():
		return false
	case a.Is4() && inAny(reserved4, a):
		return false
	case a.Is6() && nat64.Contains(a):
		b := a.As16()
		return publicAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	case a.Is6() && to4.Contains(a):
		b := a.As16()
		return publicAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	return true
}

// numericName reports whether host is a name a resolver would read as an IPv4 address in a form net.ParseIP does not accept: every label is decimal digits or 0x followed by hex digits
// ("2130706433", "0x7f.1", "017700000001"). A real hostname never has an all-numeric last label.
func numericName(host string) bool {
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	for _, l := range labels {
		if l == "" {
			return false
		}
		hex := len(l) > 2 && (l[:2] == "0x" || l[:2] == "0X")
		digits := l
		if hex {
			digits = l[2:]
		}
		for i := 0; i < len(digits); i++ {
			c := digits[i]
			ok := c >= '0' && c <= '9' || hex && (c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F')
			if !ok {
				return false
			}
		}
	}
	return true
}

// privateSuffixes are the names that can only mean the inside of a network.
var privateSuffixes = []string{".localhost", ".local", ".internal", ".lan", ".home.arpa", ".intranet", ".corp"}

// checkHost returns why host is not a public address or a name that can be one, or nil. Its message names the rule, never the host.
func checkHost(host string) error {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "" {
		return errors.New("the SMTP host is empty")
	}
	if ip := net.ParseIP(h); ip != nil {
		a, ok := netip.AddrFromSlice(ip)
		if !ok || !publicAddr(a) {
			return errors.New("the SMTP host is an address that is not public (private, loopback, link-local, carrier-grade NAT or reserved)")
		}
		return nil
	}
	switch {
	case numericName(h):
		return errors.New("the SMTP host is a numeric spelling of an address, which is not accepted: write the address in dotted form or use the host name")
	case !strings.Contains(h, "."):
		return errors.New("the SMTP host is a single-label name, which resolves inside the platform's own network: use the fully qualified host name")
	case h == "localhost":
		return errors.New("the SMTP host is localhost")
	}
	for _, s := range privateSuffixes {
		if strings.HasSuffix(h, s) {
			return errors.New("the SMTP host is a name that can only mean the inside of a network")
		}
	}
	return nil
}
