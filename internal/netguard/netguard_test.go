package netguard

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestPublic(t *testing.T) {
	tests := map[string]bool{
		"8.8.8.8":            true,
		"142.250.74.110":     true,
		"2a00:1450:4001::1":  true,
		"127.0.0.1":          false,
		"::1":                false,
		"10.1.2.3":           false,
		"172.16.0.1":         false,
		"192.168.0.1":        false,
		"169.254.169.254":    false, // a cloud's metadata service
		"100.64.0.1":         false, // carrier-grade NAT
		"0.0.0.0":            false,
		"::":                 false,
		"fe80::1":            false,
		"fc00::1":            false,
		"224.0.0.1":          false,
		"::ffff:127.0.0.1":   false, // loopback written as IPv6
		"::ffff:192.168.0.1": false,
		"198.18.0.1":         false,
		"64:ff9b::a00:1":     false, // 10.0.0.1 through NAT64
		"64:ff9b::808:808":   true,  // 8.8.8.8 through NAT64
	}
	for in, want := range tests {
		if got := Public(netip.MustParseAddr(in)); got != want {
			t.Errorf("Public(%s) = %v, want %v", in, got, want)
		}
	}
}

func TestCheckLink(t *testing.T) {
	for _, link := range []string{"http://127.0.0.1:2333/v4/info", "http://[::1]/", "https://192.168.0.1/admin", "http://169.254.169.254/latest/meta-data/"} {
		if err := CheckLink(context.Background(), link); !errors.Is(err, ErrPrivate) {
			t.Errorf("CheckLink(%s) = %v, want ErrPrivate", link, err)
		}
	}
	if err := CheckLink(context.Background(), "https://8.8.8.8/"); err != nil {
		t.Errorf("a public address was refused: %v", err)
	}
	if err := CheckLink(context.Background(), "no host"); err == nil {
		t.Error("a link without a host passed")
	}
}

func TestClientKeepsOffThePrivateNetwork(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "the router's admin page")
	}))
	defer srv.Close()

	// An *http.Transport gets a dialer that refuses the address.
	if _, err := Client(srv.Client()).Get(srv.URL); !errors.Is(err, ErrPrivate) {
		t.Errorf("guarded client reached %s: %v", srv.URL, err)
	}
	// Unguarded, the server is there. Redirects are dialed through the same
	// dialer, so a public page can't send the guarded client here either.
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// Another kind of transport has each request's host checked instead.
	var sent bool
	other := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		sent = true
		return nil, errors.New("sent")
	})}
	if _, err := Client(other).Get("http://127.0.0.1/"); !errors.Is(err, ErrPrivate) || sent {
		t.Errorf("guarded custom transport: sent %v, err %v; want it refused before sending", sent, err)
	}
}

func TestDialerChecksEachAddress(t *testing.T) {
	control := Dialer().Control
	if err := control("tcp4", "8.8.8.8:443", nil); err != nil {
		t.Errorf("a public address was refused: %v", err)
	}
	for _, addr := range []string{"127.0.0.1:2333", "[::1]:80", "[::ffff:10.0.0.1]:80", "192.168.0.1:443"} {
		if err := control("tcp", addr, nil); !errors.Is(err, ErrPrivate) {
			t.Errorf("dialing %s: %v, want ErrPrivate", addr, err)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientClearsTLSDialHooks(t *testing.T) {
	// HTTPS dials with these when they're set, which would skip the check.
	for _, hook := range []string{"DialTLSContext", "DialTLS"} {
		t.Run(hook, func(t *testing.T) {
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			var called bool
			dial := func(string, string) (net.Conn, error) {
				called = true
				return nil, errors.New("dialed without the check")
			}
			if hook == "DialTLSContext" {
				transport.DialTLSContext = func(_ context.Context, network, addr string) (net.Conn, error) { return dial(network, addr) }
			} else {
				transport.DialTLS = dial
			}
			_, err := Client(&http.Client{Transport: transport}).Get("https://127.0.0.1/private")
			if called || !errors.Is(err, ErrPrivate) {
				t.Errorf("through %s: dialed %v, err %v; want it refused", hook, called, err)
			}
		})
	}
}
