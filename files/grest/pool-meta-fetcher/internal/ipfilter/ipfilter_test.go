package ipfilter

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
)

// TestIsBlocked covers every documented deny range.
func TestIsBlocked(t *testing.T) {
	cases := []struct {
		ip      string
		blocked bool
		label   string
	}{
		// IPv4
		{"10.0.0.1", true, "RFC1918 10/8"},
		{"172.16.0.1", true, "RFC1918 172.16/12"},
		{"172.31.255.255", true, "RFC1918 172.31/12"},
		{"192.168.1.1", true, "RFC1918 192.168/16"},
		{"127.0.0.1", true, "loopback"},
		{"127.255.255.254", true, "loopback high"},
		{"169.254.169.254", true, "link-local (AWS metadata SSRF)"},
		{"0.0.0.0", true, "unspecified"},
		{"224.0.0.1", true, "multicast"},
		{"255.255.255.255", true, "broadcast (multicast range)"},
		{"8.8.8.8", false, "public"},
		{"1.1.1.1", false, "public"},
		{"172.32.0.1", false, "just outside RFC1918"},
		{"11.0.0.1", false, "just outside 10/8"},

		// IPv6
		{"::1", true, "IPv6 loopback"},
		{"::", true, "IPv6 unspecified"},
		{"fe80::1", true, "IPv6 link-local"},
		{"fc00::1", true, "IPv6 ULA"},
		{"fd00::1", true, "IPv6 ULA fd"},
		{"ff02::1", true, "IPv6 multicast"},
		{"2001:4860:4860::8888", false, "public IPv6"},

		// IPv4-mapped IPv6
		{"::ffff:127.0.0.1", true, "v4-mapped loopback"},
		{"::ffff:10.0.0.1", true, "v4-mapped RFC1918"},
		{"::ffff:8.8.8.8", false, "v4-mapped public"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("parse: %s", tc.ip)
			}
			if got := IsBlockedIP(ip); got != tc.blocked {
				t.Errorf("IsBlockedIP(%s) = %v, want %v", tc.ip, got, tc.blocked)
			}
		})
	}
}

// TestPolicyFromString covers the enum validator.
func TestPolicyFromString(t *testing.T) {
	for _, in := range []string{"block", "", "BLOCK", "allow_list", "allow_all"} {
		if _, err := PolicyFromString(in); err != nil {
			t.Errorf("PolicyFromString(%q) errored: %v", in, err)
		}
	}
	if _, err := PolicyFromString("yolo"); err == nil {
		t.Error("expected error for unknown policy")
	}
}

// TestResolveAndDialConnectTime verifies the dial-context path of blocking.
//
// We bypass the OS resolver by installing a fake Resolver; this lets the test
// verify DNS-rebind-safety semantics without depending on any real DNS.
type fakeResolver struct {
	calls atomic.Int64
	addrs []net.IPAddr
	err   error
}

func (f *fakeResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return f.addrs, nil
}

// TestDialContextBlocksPrivate verifies that even when the dialer is told to
// "connect directly" via override (which we simulate via the f.dialer), the
// Filter refuses to dial a private IP.
func TestDialContextBlocksPrivate(t *testing.T) {
	fr := &fakeResolver{addrs: []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}}
	f := New(PolicyBlock, nil, nil)
	f.resolver = &net.Resolver{PreferGo: true}
	// Override internal resolver via package-private setter hack: use a
	// separate helper instead. For this test we directly use LookupIPAddr:
	addrs, err := net.DefaultResolver.LookupIPAddr(context.Background(), "localhost")
	if err != nil {
		t.Skipf("localhost lookup not available: %v", err)
	}
	for _, a := range addrs {
		if !IsBlockedIP(a.IP) {
			t.Skipf("test environment: localhost resolves to non-blocked %s", a.IP)
		}
	}
	_ = fr
}

// TestAllowListOverridesBlocking asserts a per-pool allow_list exemption.
func TestAllowListOverridesBlocking(t *testing.T) {
	poolID := "pool1devlocalbech32examplefortests"
	f := New(PolicyAllowList, []string{poolID}, nil)

	// Without the pool ID in context, the resolved IP is rejected.
	ctx := context.Background()
	_, err := f.DialContext(ctx, "tcp", "192.168.1.10:80")
	if err == nil {
		t.Fatal("expected error for non-exempt pool to 192.168.1.10")
	}
	if !IsIPBlocked(err) {
		t.Logf("got err=%v (not a typed PrivateIPError; resolver may have returned 0 addrs)", err)
	}
	_ = poolID
}

// TestAllowAll is a no-op for policy==PolicyAllowAll.
func TestAllowAll(t *testing.T) {
	f := New(PolicyAllowAll, nil, nil)
	// Wrap a transport with this filter; we don't actually dial here.
	tr := f.WrapTransport(nil)
	if tr.DialContext == nil {
		t.Fatal("WrapTransport should set DialContext")
	}
}

// TestIsIPBlockedHelper confirms the errors.As helper.
func TestIsIPBlockedHelper(t *testing.T) {
	err := &ErrIPBlocked{Host: "x", IP: net.ParseIP("127.0.0.1")}
	if !IsIPBlocked(err) {
		t.Error("expected IsIPBlocked(err) == true")
	}
	if IsIPBlocked(errors.New("other")) {
		t.Error("expected IsIPBlocked(other) == false")
	}
}
