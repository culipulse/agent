package main

import (
	"errors"
	"log"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"time"
)

// sharedGuard is true once this process is known to run as a shared (first-party) agent, i.e. it
// probes tenant-chosen targets from CuliPulse infrastructure. Then every outbound dial refuses
// private/reserved destinations — checked on the resolved IP at connect time, so it covers the
// initial target, every redirect hop and DNS rebinding in one place. Sticky: never turned off.
// Tenant-owned agents leave it off; reaching private hosts is their job.
var sharedGuard atomic.Bool

// enableSharedGuard turns the shared-agent dial guard on. Idempotent and sticky — once on, it is
// never turned back off. reason is a short human string for the one-time log line ("server" when the
// worker's /pull response reports this agent as first_party, "env" when CULIPULSE_SHARED_AGENT=1
// forces it on, "test" from tests exercising this path directly).
//
// On the FIRST off->on flip only (guarded by CompareAndSwap so a second call, or a concurrent one, is
// a no-op), it also closes sharedTransport's pooled idle connections. Those may have been dialed
// before the guard existed, so they were never checked by guardControl — reusing one from the pool
// would silently bypass the guard for that request. Discarding them forces every subsequent probe to
// dial fresh, through the guard.
func enableSharedGuard(reason string) {
	if sharedGuard.CompareAndSwap(false, true) {
		sharedTransport.CloseIdleConnections()
		log.Printf("shared-agent dial guard: on (%s)", reason)
	}
}

// applyAgentKind turns the shared guard on when the server reports this agent as first_party on
// /pull. Called from Client.Pull after a successful decode, before any work items in that same
// response are ever handed to a prober, so the guard is active before any tenant-chosen target from
// this pull can be dialed. A "tenant" (or any other/empty) kind is a no-op — see enableSharedGuard for
// why that never turns an already-on guard back off.
func applyAgentKind(kind string) {
	if kind == "first_party" {
		enableSharedGuard("server")
	}
}

// sharedGuardFromEnv reports whether CULIPULSE_SHARED_AGENT forces the guard on. This is
// defence-in-depth for a shared-agent host: the guard is on from process start, independent of (and
// ahead of) whatever the server reports on the first /pull.
func sharedGuardFromEnv() bool {
	return os.Getenv("CULIPULSE_SHARED_AGENT") == "1"
}

var errBlockedTarget = errors.New("blocked_target: destination is a private/reserved address")

var blockedNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
		"172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/4", "::/128", "::1/128", "fe80::/10", "fc00::/7", "ff00::/8"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// ipBlockedForShared reports whether a shared agent must not connect to ip.
func ipBlockedForShared(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil { // also unwraps ::ffff:a.b.c.d
		ip = v4
	}
	for _, n := range blockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// guardControl is a net.Dialer.Control hook: it runs after DNS resolution, right before connect,
// with the literal ip:port about to be dialed.
func guardControl(network, address string, _ syscall.RawConn) error {
	if !sharedGuard.Load() {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errBlockedTarget
	}
	if ipBlockedForShared(net.ParseIP(host)) {
		return errBlockedTarget
	}
	return nil
}

func guardedDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: guardControl}
}
