package main

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// withGuard enables the shared guard for one test and restores it afterwards.
func withGuard(t *testing.T) {
	t.Helper()
	prev := sharedGuard.Load()
	sharedGuard.Store(true)
	t.Cleanup(func() { sharedGuard.Store(prev) })
}

func TestIPBlockedForShared(t *testing.T) {
	blocked := []string{"127.0.0.1", "0.0.0.0", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "100.127.255.255", "224.0.0.1", "::1", "::", "fe80::1", "fc00::1",
		"fd12::3", "ff02::1", "::ffff:127.0.0.1", "::ffff:a9fe:a9fe"}
	for _, s := range blocked {
		if !ipBlockedForShared(net.ParseIP(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "172.15.0.1", "172.32.0.1", "100.63.0.1", "100.128.0.1", "2606:4700:4700::1111", "::ffff:8.8.8.8"}
	for _, s := range allowed {
		if ipBlockedForShared(net.ParseIP(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestSharedGuard_BlocksRedirectToLoopback(t *testing.T) {
	var internalHits atomic.Int32
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { internalHits.Add(1) }))
	defer internal.Close()
	// The "public" hop is also on loopback (httptest limitation), so with the guard on even the FIRST dial is refused.
	// That still proves the guard sits on the transport every hop uses; the redirect hop specifically is
	// covered by TestSharedGuard_RefusesRedirectHop below, which uses distinct addresses for each hop.
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	defer public.Close()
	withGuard(t)
	res := probe(WorkItem{MonitorID: "m", Type: "http", Target: public.URL, TimeoutMs: 2000, Method: "GET", ExpectedStatus: "2xx", FollowRedirects: true})
	if res.OK || res.Cause == nil || *res.Cause != "blocked_target" {
		t.Fatalf("want blocked_target, got ok=%v cause=%v", res.OK, res.Cause)
	}
	if internalHits.Load() != 0 {
		t.Fatalf("internal server was reached %d times", internalHits.Load())
	}
}

func TestSharedGuard_ControlRejectsEveryHop(t *testing.T) {
	// guardControl is what net/http calls on every new connection, including each redirect hop.
	withGuard(t)
	for _, addr := range []string{"127.0.0.1:80", "[::ffff:127.0.0.1]:80", "169.254.169.254:80", "[::1]:443"} {
		if err := guardControl("tcp4", addr, nil); err == nil {
			t.Errorf("%s: want error", addr)
		}
	}
	if err := guardControl("tcp4", "8.8.8.8:443", nil); err != nil {
		t.Errorf("public addr rejected: %v", err)
	}
}

func TestSharedGuard_HostnameResolvingToLoopbackBlocked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	withGuard(t)
	res := probe(WorkItem{MonitorID: "m", Type: "http", Target: "http://localhost:" + port + "/", TimeoutMs: 2000, Method: "GET", ExpectedStatus: "2xx"})
	if res.Cause == nil || *res.Cause != "blocked_target" {
		t.Fatalf("want blocked_target, got %v", res.Cause)
	}
}

func TestSharedGuard_DiagnoseTransportGuarded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	withGuard(t)
	res := probeDiagnose(WorkItem{MonitorID: "m", Type: "http", Target: srv.URL, TimeoutMs: 2000, Method: "GET", ExpectedStatus: "2xx", RequestID: "r"})
	if res.Cause == nil || *res.Cause != "blocked_target" {
		t.Fatalf("want blocked_target, got %v", res.Cause)
	}
}

func TestGuardOff_TenantStillReachesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	// sharedGuard is false by default (no withGuard).
	res := probe(WorkItem{MonitorID: "m", Type: "http", Target: srv.URL, TimeoutMs: 2000, Method: "GET", ExpectedStatus: "2xx"})
	if !res.OK {
		t.Fatalf("tenant-mode probe to loopback should succeed, cause=%v", res.Cause)
	}
}

func TestSharedGuard_TCPBlocked(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	withGuard(t)
	res := tcpProbe(WorkItem{MonitorID: "m", Type: "tcp", Target: ln.Addr().String(), TimeoutMs: 1000})
	if res.Cause == nil || *res.Cause != "blocked_target" {
		t.Fatalf("want blocked_target, got %v", res.Cause)
	}
}

func TestSharedGuard_UDPBlocked(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	withGuard(t)
	res := udpProbe(WorkItem{MonitorID: "m", Type: "udp", Target: pc.LocalAddr().String(), TimeoutMs: 500})
	if res.Cause == nil || *res.Cause != "blocked_target" {
		t.Fatalf("want blocked_target, got %v", res.Cause)
	}
}

func TestSharedGuard_ICMPBlockedBeforeSocket(t *testing.T) {
	withGuard(t)
	// Must be refused on the resolved IP before opening a raw socket (so this works unprivileged).
	res := icmpProbe(WorkItem{MonitorID: "m", Type: "icmp", Target: "127.0.0.1", TimeoutMs: 500})
	if res.Cause == nil || *res.Cause != "blocked_target" {
		t.Fatalf("want blocked_target, got %v", res.Cause)
	}
}

// TestEnableSharedGuardClosesIdlePooledConnections is a regression guard for the CloseIdleConnections
// call on the first off->on flip: sharedTransport pools keep-alive connections per host, and a
// connection dialed before the guard existed was never checked by guardControl, so reusing it would
// silently bypass the guard for that request. It proves the pool was actually flushed — via the
// server observing the pooled socket close — rather than merely that new dials are blocked
// afterwards (already covered by the other tests in this file).
func TestEnableSharedGuardClosesIdlePooledConnections(t *testing.T) {
	prev := sharedGuard.Load()
	t.Cleanup(func() { sharedGuard.Store(prev) })
	sharedGuard.Store(false)

	var closed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()

	// Warm sharedTransport's idle pool with a normal request — the SAME transport every real HTTP
	// probe uses (buildClient wraps it) — body fully drained and closed so the connection is
	// eligible for reuse, not left mid-read.
	client := &http.Client{Transport: sharedTransport}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("warm-up request failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	// Closing resp.Body only signals the transport's readLoop goroutine to park the connection as
	// idle; it doesn't happen inline on this call. Give it a moment before acting.
	time.Sleep(100 * time.Millisecond)

	enableSharedGuard("test")

	// CloseIdleConnections closes the pooled socket synchronously from the client side; give the
	// server a short window to observe it. Deliberately no second request is made here — that would
	// also be refused by guardControl now that the guard is on, which would prove the wrong thing.
	deadline := time.Now().Add(2 * time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if closed.Load() == 0 {
		t.Fatal("want the pooled idle connection closed by enableSharedGuard, got none closed")
	}
}

// classifyError (and classifyTCPError/classifyUDPError) must key off error IDENTITY
// (errors.Is(err, errBlockedTarget)), never off substring-matching the error text. A plain
// string match spoofs: here the guard is OFF (tenant mode, nothing should ever be "blocked"),
// and the target URL's path merely CONTAINS the text "blocked_target" — net/http wraps the dial
// failure in a *url.Error whose Error() string embeds the request URL, so a naive
// strings.Contains(s, "blocked_target") check misreads the URL text as the guard's cause.
// Port 1 has no listener, so this is an ordinary connection refusal, not a guard block.
func TestClassifyError_NoSpoofFromTargetURLText(t *testing.T) {
	res := probe(WorkItem{MonitorID: "m", Type: "http", Target: "http://127.0.0.1:1/blocked_target", TimeoutMs: 1000, Method: "GET", ExpectedStatus: "2xx"})
	if res.Cause != nil && *res.Cause == "blocked_target" {
		t.Fatalf("cause spoofed to blocked_target by the target URL text, got ok=%v cause=%v", res.OK, res.Cause)
	}
}

// TestSharedGuard_RefusesRedirectHop proves the guard blocks a redirect to a DIFFERENT (internal)
// address, not just the initial target — the earlier BlocksRedirectToLoopback test can't show this
// because both hops there share loopback. This uses two distinct addresses: 127.0.0.1 ("public",
// allowed) redirecting to 127.0.0.2 ("internal", blocked via a narrowed blockedNets). It must fail
// if DialContext is removed from sharedTransport (proved manually; not committed as a mutation).
func TestSharedGuard_RefusesRedirectHop(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.2:0")
	if err != nil {
		t.Skip("127.0.0.2 not bindable")
	}
	var internalHits atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { internalHits.Add(1) })
	srv := httptest.NewUnstartedServer(h)
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	var publicHits atomic.Int32
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		publicHits.Add(1)
		http.Redirect(w, r, srv.URL, http.StatusFound)
	}))
	defer public.Close()

	// Swapping the package-level blockedNets is only safe because no test in this package calls
	// t.Parallel() — a parallel test reading blockedNets concurrently with this swap would race.
	prevNets := blockedNets
	_, only, _ := net.ParseCIDR("127.0.0.2/32")
	blockedNets = []*net.IPNet{only}
	t.Cleanup(func() { blockedNets = prevNets })

	withGuard(t)
	res := probe(WorkItem{MonitorID: "m", Type: "http", Target: public.URL, TimeoutMs: 2000, Method: "GET", ExpectedStatus: "2xx", FollowRedirects: true})
	if res.Cause == nil || *res.Cause != "blocked_target" {
		t.Fatalf("want blocked_target, got ok=%v cause=%v", res.OK, res.Cause)
	}
	if publicHits.Load() != 1 {
		t.Fatalf("public server hit %d times, want exactly 1", publicHits.Load())
	}
	if internalHits.Load() != 0 {
		t.Fatalf("internal server was reached %d times, want 0", internalHits.Load())
	}
}
