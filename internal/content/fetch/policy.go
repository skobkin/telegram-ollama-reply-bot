package fetch

import (
	"fmt"
	"net"
	"strings"

	"golang.org/x/net/idna"
)

// policy is the deny-by-default network policy applied to every connection
// the fetcher makes. It blocks the non-public IP ranges that make SSRF
// possible (loopback, private, link-local, cloud metadata, CGNAT, multicast,
// unspecified and broadcast addresses) plus operator-added CIDR ranges and
// domains. There is deliberately no allow-list: DENY_* settings extend the
// built-in ranges, they never replace them.
type policy struct {
	deniedRanges []*net.IPNet
	denyDomains  []string
}

// defaultDeniedRanges returns the built-in blocked ranges. Everything listed
// here is unreachable for fetched URLs even when DNS or a redirect points at
// it, unless a proxy carries the connection (see fetcher.go for the proxy
// tradeoff and its follow-up issue).
func defaultDeniedRanges() []*net.IPNet {
	cidrs := []string{
		// IPv4 loopback, private (RFC 1918), link-local (RFC 3927, includes
		// cloud metadata endpoints), CGNAT (RFC 6598), this-network,
		// limited broadcast.
		"127.0.0.0/8",
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"169.254.0.0/16",
		"100.64.0.0/10",
		"0.0.0.0/8",
		"255.255.255.255/32",
		// IPv4 multicast.
		"224.0.0.0/4",
		// IPv6 loopback, link-local, unique-local, multicast, unspecified.
		"::1/128",
		"fe80::/10",
		"fc00::/7",
		"ff00::/8",
		"::/128",
	}

	ranges := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, parsed, err := net.ParseCIDR(cidr)
		if err != nil {
			// Unreachable: the literals above are compile-time constants.
			panic(fmt.Sprintf("fetch: builtin cidr %q does not parse: %v", cidr, err))
		}
		ranges = append(ranges, parsed)
	}

	return ranges
}

// parseCIDRs parses operator-provided CIDR strings ("203.0.113.0/24",
// "2001:db8::/32", or bare addresses) into networks. An invalid entry is an
// error so configuration mistakes refuse the boot instead of silently not
// applying.
func parseCIDRs(raw []string) ([]*net.IPNet, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	ranges := make([]*net.IPNet, 0, len(raw))
	for _, entry := range raw {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		// Bare addresses are accepted as /32 or /128 ranges for convenience.
		if !strings.Contains(entry, "/") {
			if strings.Contains(entry, ":") {
				entry += "/128"
			} else {
				entry += "/32"
			}
		}

		_, parsed, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, err
		}
		ranges = append(ranges, parsed)
	}

	return ranges, nil
}

// allowedIP reports whether a resolved address may be dialed. IPv4-mapped
// IPv6 addresses (::ffff:127.0.0.1) are normalized to their 4-byte form first
// so they match the IPv4 ranges they really belong to; anything unparseable
// or empty is denied (fail closed).
func (p *policy) allowedIP(ip net.IP) bool {
	if ip == nil {
		return false
	}

	// To4() maps 4-in-6 forms (and deprecated IPv4-compatible addresses) to
	// the 4-byte representation; a genuine IPv6 address stays 16 bytes.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	for _, denied := range p.deniedRanges {
		if denied.Contains(ip) {
			return false
		}
	}

	return true
}

// allowedHostname reports whether a URL host may be requested. IP-literal
// hosts are checked against the IP ranges; names are lowercased, converted to
// their ASCII form (IDN), and matched against the configured deny list on
// domain boundaries ("example.org" denies "sub.example.org" but not
// "notexample.org"). Hostnames that cannot be converted are denied (fail
// closed).
func (p *policy) allowedHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return false
	}

	// IP literals never reach the domain list.
	if ip := net.ParseIP(host); ip != nil {
		return p.allowedIP(ip)
	}

	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" {
		return false
	}
	host = strings.TrimSuffix(strings.ToLower(ascii), ".")

	for _, denied := range p.denyDomains {
		denied = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(denied)), ".")
		if denied == "" {
			continue
		}
		// Deny entries may be written in Unicode form; both sides are
		// compared in their ASCII (punycode) representation.
		deniedASCII, err := idna.Lookup.ToASCII(denied)
		if err == nil && deniedASCII != "" {
			denied = strings.TrimSuffix(strings.ToLower(deniedASCII), ".")
		}
		if host == denied || strings.HasSuffix(host, "."+denied) {
			return false
		}
	}

	return true
}
