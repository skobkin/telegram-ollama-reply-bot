package fetch

import (
	"net"
	"testing"
)

func TestPolicyDeniedIPRanges(t *testing.T) {
	t.Parallel()

	p := &policy{deniedRanges: defaultDeniedRanges()}

	denied := []string{
		"127.0.0.1",        // IPv4 loopback
		"127.8.8.8",        // IPv4 loopback, non-obvious octet
		"::1",              // IPv6 loopback
		"::ffff:127.0.0.1", // IPv4-mapped loopback
		"10.1.2.3",         // RFC 1918
		"172.16.0.1",       // RFC 1918 lower bound
		"172.31.255.255",   // RFC 1918 upper bound
		"192.168.1.1",      // RFC 1918
		"169.254.169.254",  // link-local, cloud metadata
		"100.64.0.1",       // CGNAT lower bound
		"100.127.255.255",  // CGNAT upper bound
		"0.0.0.0",          // this-network
		"255.255.255.255",  // limited broadcast
		"224.0.0.1",        // IPv4 multicast
		"fe80::1",          // IPv6 link-local
		"fe80::",           // IPv6 link-local base
		"fc00::1",          // IPv6 unique-local
		"fd12:3456::1",     // IPv6 unique-local (fd00::/8 inside fc00::/7)
		"ff02::1",          // IPv6 multicast
		"::",               // unspecified
	}

	for _, raw := range denied {
		if p.allowedIP(net.ParseIP(raw)) {
			t.Errorf("expected %s to be denied", raw)
		}
	}

	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34",
		"126.255.255.255", // just below loopback
		"128.0.0.1",       // just above loopback
		"172.15.255.255",  // just below RFC 1918 172.16/12
		"172.32.0.0",      // just above RFC 1918 172.16/12
		"100.63.255.255",  // just below CGNAT
		"100.128.0.0",     // just above CGNAT
		"2606:4700:4700::1111",
		"2001:4860:4860::8888",
	}

	for _, raw := range allowed {
		if !p.allowedIP(net.ParseIP(raw)) {
			t.Errorf("expected %s to be allowed", raw)
		}
	}

	if p.allowedIP(nil) {
		t.Error("expected nil IP to be denied")
	}
}

func TestPolicyUserCIDRs(t *testing.T) {
	t.Parallel()

	denied, err := parseCIDRs([]string{"203.0.113.0/24", "2001:db8::/32", "  192.0.2.10  "})
	if err != nil {
		t.Fatalf("parseCIDRs returned error: %v", err)
	}

	p := &policy{deniedRanges: append(defaultDeniedRanges(), denied...)}

	blocked := []string{
		"203.0.113.5",
		"203.0.113.200",
		"2001:db8::1",
		"2001:db8:aaaa::1",
		"192.0.2.10", // bare address parsed as /32
	}
	for _, raw := range blocked {
		if p.allowedIP(net.ParseIP(raw)) {
			t.Errorf("expected user-denied %s to be blocked", raw)
		}
	}

	if !p.allowedIP(net.ParseIP("203.0.114.5")) {
		t.Error("expected address outside user CIDR to be allowed")
	}

	if _, err := parseCIDRs([]string{"not-a-cidr"}); err == nil {
		t.Fatal("expected invalid CIDR string to error")
	}

	if _, err := parseCIDRs(nil); err != nil {
		t.Fatalf("parseCIDRs(nil) returned error: %v", err)
	}
}

func TestPolicyBareAddressParsing(t *testing.T) {
	t.Parallel()

	v4, err := parseCIDRs([]string{"192.0.2.1"})
	if err != nil {
		t.Fatalf("bare IPv4 address errored: %v", err)
	}
	if got := v4[0].String(); got != "192.0.2.1/32" {
		t.Fatalf("expected /32 for bare IPv4, got %s", got)
	}

	v6, err := parseCIDRs([]string{"2001:db8::1"})
	if err != nil {
		t.Fatalf("bare IPv6 address errored: %v", err)
	}
	if got := v6[0].String(); got != "2001:db8::1/128" {
		t.Fatalf("expected /128 for bare IPv6, got %s", got)
	}
}

func TestPolicyDenyDomains(t *testing.T) {
	t.Parallel()

	p := &policy{
		deniedRanges: defaultDeniedRanges(),
		denyDomains:  []string{"Example.org", "blocked.example"},
	}

	denied := []string{
		"example.org",
		"EXAMPLE.ORG",
		"sub.example.org",
		"deep.sub.example.org",
		"blocked.example",
		"www.blocked.example",
	}
	for _, host := range denied {
		if p.allowedHostname(host) {
			t.Errorf("expected %q to be denied", host)
		}
	}

	allowed := []string{
		"notexample.org",       // boundary: no domain-label match
		"example.org.evil.com", // suffix of a different domain
		"other.example",
		"example.com",
		"xn--bcher-kva.example", // punycode of bücher.example, not on the list
	}
	for _, host := range allowed {
		if !p.allowedHostname(host) {
			t.Errorf("expected %q to be allowed", host)
		}
	}
}

func TestPolicyDenyDomainsUnicode(t *testing.T) {
	t.Parallel()

	p := &policy{denyDomains: []string{"bücher.example"}}

	if p.allowedHostname("www.bücher.example") {
		t.Error("expected unicode subdomain of denied IDN to be denied")
	}
	if p.allowedHostname("BÜCHER.example") {
		t.Error("expected case-folded unicode form of denied IDN to be denied")
	}
	if !p.allowedHostname("bücher.other.example") {
		t.Error("expected unicode domain outside deny list to be allowed")
	}
}

func TestPolicyIPHostsAndGarbage(t *testing.T) {
	t.Parallel()

	p := &policy{deniedRanges: defaultDeniedRanges()}

	if p.allowedHostname("127.0.0.1") {
		t.Error("expected literal loopback host to be denied")
	}
	if p.allowedHostname("::1") {
		t.Error("expected literal IPv6 loopback host to be denied")
	}
	if !p.allowedHostname("93.184.216.34") {
		t.Error("expected literal public host to be allowed")
	}
	if p.allowedHostname("") {
		t.Error("expected empty host to be denied")
	}
	if p.allowedHostname("fe80::1%eth0") {
		t.Error("expected zoned address to be denied (fail closed)")
	}
	if p.allowedHostname("in valid host") {
		t.Error("expected host with spaces to be denied (fail closed)")
	}
}
