// Package netguard keeps lullaby off the bot host's own network. A link from
// a stranger in a public server could otherwise make the bot fetch its owner's
// router, other machines at home, or a cloud provider's metadata service.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// ErrPrivate is returned for a link to an address that isn't on the public
// internet.
var ErrPrivate = errors.New("on a private network")

// lookupFor is how long resolving a host may take.
const lookupFor = 5 * time.Second

// reserved are ranges that aren't private by Go's reckoning, but aren't the
// public internet either.
var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fec0::/10"),
}

// nat64 is the prefix NAT64 puts IPv4 addresses in: 64:ff9b::10.0.0.1 reaches
// 10.0.0.1 where a network translates them.
var nat64 = netip.MustParsePrefix("64:ff9b::/96")

// Public reports whether ip is on the public internet.
func Public(ip netip.Addr) bool {
	ip = ip.Unmap() // ::ffff:127.0.0.1 is 127.0.0.1
	if nat64.Contains(ip) {
		b := ip.As16()
		return Public(netip.AddrFrom4([4]byte(b[12:])))
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false // loopback, link-local, multicast and unspecified aren't global unicast
	}
	for _, p := range reserved {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// resolve returns host's addresses, or ErrPrivate if any of them isn't
// public: a name that points at both is refused.
func resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		if !Public(ip) {
			return nil, fmt.Errorf("%w: %s", ErrPrivate, host)
		}
		return []netip.Addr{ip}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, lookupFor)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%s has no addresses", host)
	}
	for _, ip := range ips {
		if !Public(ip) {
			return nil, fmt.Errorf("%w: %s is %s", ErrPrivate, host, ip)
		}
	}
	return ips, nil
}

// CheckLink returns ErrPrivate if link's host isn't on the public internet,
// for links handed to programs that fetch them themselves, like yt-dlp. They
// resolve the host again, and can follow redirects anywhere, so it only
// keeps the plain cases out.
func CheckLink(ctx context.Context, link string) error {
	u, err := url.Parse(link)
	if err != nil {
		return err
	}
	if u.Hostname() == "" {
		return fmt.Errorf("%q has no host", link)
	}
	_, err = resolve(ctx, u.Hostname())
	return err
}

// Dialer returns a dialer, set up as Go's default transport's, that connects
// only to public addresses. It checks each address as it's about to connect
// to it, after resolving, so a name can't point somewhere else in between,
// and Go still races IPv6 against IPv4 when a host has both.
func Dialer() *net.Dialer {
	return &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if !Public(ap.Addr()) {
				return fmt.Errorf("%w: %s", ErrPrivate, ap.Addr())
			}
			return nil
		},
	}
}

// Client returns a copy of c that connects only to public addresses. An
// *http.Transport is copied, with its own connections, and Dialer in place
// of its dial hooks, the TLS ones included. Another
// kind of transport can't be given a dialer, so each request's host is
// checked before it's sent instead. Behind a proxy, only the proxy's
// address is checked.
func Client(c *http.Client) *http.Client {
	guarded := *c
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if t, ok := base.(*http.Transport); ok {
		t = t.Clone()
		t.DialContext = Dialer().DialContext
		// HTTPS would dial with these instead; without them, it dials
		// through DialContext too, and TLS is set up from the transport's
		// own TLS settings.
		t.DialTLSContext = nil
		t.DialTLS = nil //lint:ignore SA1019 cleared so it can't bypass the check
		guarded.Transport = t
	} else {
		guarded.Transport = checked{base}
	}
	return &guarded
}

// checked checks each request's host before passing it on.
type checked struct{ base http.RoundTripper }

func (c checked) RoundTrip(r *http.Request) (*http.Response, error) {
	if _, err := resolve(r.Context(), r.URL.Hostname()); err != nil {
		return nil, err
	}
	return c.base.RoundTrip(r)
}
