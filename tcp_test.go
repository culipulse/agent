package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestTCPProbeUpDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	up := tcpProbe(WorkItem{MonitorID: "m", Type: "tcp", Target: ln.Addr().String(), TimeoutMs: 2000})
	if !up.OK || up.Cause != nil {
		t.Errorf("open port: ok=%v cause=%v", up.OK, up.Cause)
	}

	ln.Close()
	down := tcpProbe(WorkItem{MonitorID: "m", Type: "tcp", Target: ln.Addr().String(), TimeoutMs: 2000})
	if down.OK || down.Cause == nil {
		t.Errorf("closed port: expected down, got ok=%v cause=%v", down.OK, down.Cause)
	}
}

func TestProbeDispatch(t *testing.T) {
	r := probe(WorkItem{MonitorID: "m", Type: "tcp", Target: "127.0.0.1:1", TimeoutMs: 500})
	if r.OK {
		t.Errorf("tcp to closed 127.0.0.1:1 should be down")
	}
}

// tcpServer starts a localhost TCP listener. On each connection it optionally writes `banner`
// immediately, then (if echo) copies one read back to the client. Returns addr + stop().
func tcpServer(t *testing.T, banner []byte, echo bool) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed by stop()
			}
			go func(conn net.Conn) {
				defer conn.Close()
				if banner != nil {
					_, _ = conn.Write(banner)
				}
				if echo {
					buf := make([]byte, 1024)
					n, _ := conn.Read(buf)
					if n > 0 {
						_, _ = conn.Write(buf[:n])
					}
				}
				time.Sleep(50 * time.Millisecond)
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestTCPProbe_ConnectOnlyUnchanged(t *testing.T) {
	addr, stop := tcpServer(t, nil, false)
	defer stop()
	r := tcpProbe(WorkItem{MonitorID: "m", Type: "tcp", Target: addr, TimeoutMs: 1000})
	if !r.OK || r.Cause != nil {
		t.Errorf("connect-only: expected up, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

func TestTCPProbe_BannerMatch(t *testing.T) {
	addr, stop := tcpServer(t, []byte("SSH-2.0-OpenSSH_9.6"), false)
	defer stop()
	cs := CheckSpec{Transport: &Transport{Expect: "SSH-"}}
	r := tcpProbe(WorkItem{MonitorID: "m", Type: "tcp", Target: addr, TimeoutMs: 1000, CheckSpec: cs})
	if !r.OK || r.Cause != nil {
		t.Errorf("banner match: expected up, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

func TestTCPProbe_BannerMismatch(t *testing.T) {
	addr, stop := tcpServer(t, []byte("220 mail.example.com ESMTP"), false)
	defer stop()
	cs := CheckSpec{Transport: &Transport{Expect: "+PONG"}}
	r := tcpProbe(WorkItem{MonitorID: "m", Type: "tcp", Target: addr, TimeoutMs: 1000, CheckSpec: cs})
	if r.OK || r.Cause == nil || *r.Cause != "tcp_mismatch" {
		t.Errorf("mismatch: expected down tcp_mismatch, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

func TestTCPProbe_NoBanner(t *testing.T) {
	addr, stop := tcpServer(t, nil, false) // accepts, never writes
	defer stop()
	cs := CheckSpec{Transport: &Transport{Expect: "SSH-"}}
	r := tcpProbe(WorkItem{MonitorID: "m", Type: "tcp", Target: addr, TimeoutMs: 300, CheckSpec: cs})
	if r.OK || r.Cause == nil || *r.Cause != "tcp_no_banner" {
		t.Errorf("silence: expected down tcp_no_banner, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

func TestTCPProbe_SendThenEchoMatch(t *testing.T) {
	addr, stop := tcpServer(t, nil, true) // echoes what we send
	defer stop()
	cs := CheckSpec{Transport: &Transport{Payload: "PING\r\n", Expect: "PING"}}
	r := tcpProbe(WorkItem{MonitorID: "m", Type: "tcp", Target: addr, TimeoutMs: 1000, CheckSpec: cs})
	if !r.OK || r.Cause != nil {
		t.Errorf("send+echo: expected up, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

// chunkServer accepts connections and writes each chunk in order with `gap` between them, then
// (when hold) keeps the connection open until the client goes away; otherwise closes after the last.
func chunkServer(t *testing.T, chunks []string, gap time.Duration, hold bool) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				for i, ch := range chunks {
					if i > 0 {
						time.Sleep(gap)
					}
					if _, err := conn.Write([]byte(ch)); err != nil {
						return
					}
				}
				if hold {
					buf := make([]byte, 16)
					_, _ = conn.Read(buf) // blocks until the probe closes
				}
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func tcpExpectItem(addr, expect string, timeoutMs int) WorkItem {
	return WorkItem{MonitorID: "m", Type: "tcp", Target: addr, TimeoutMs: timeoutMs,
		CheckSpec: CheckSpec{Transport: &Transport{Expect: expect}}}
}

// #389: a multi-line SMTP greeting arrives in two segments; the expected text is in the second.
func TestTCPProbe_SplitGreetingMatchesLaterChunk(t *testing.T) {
	addr, stop := chunkServer(t, []string{"220-mail.example.com ESMTP\r\n", "220 ready\r\n"}, 300*time.Millisecond, true)
	defer stop()
	r := tcpProbe(tcpExpectItem(addr, "220 ready", 3000))
	if !r.OK || r.Cause != nil {
		t.Fatalf("split greeting: expected up, got ok=%v cause=%v", r.OK, r.Cause)
	}
	if r.LatencyMs >= 2000 {
		t.Errorf("a matching banner must return as soon as it matches, took %dms", r.LatencyMs)
	}
}

// The expected text straddles two segments ("SS" | "H-2.0"): matching runs on the accumulated bytes.
func TestTCPProbe_ExpectStraddlesChunks(t *testing.T) {
	addr, stop := chunkServer(t, []string{"SS", "H-2.0-OpenSSH"}, 100*time.Millisecond, false)
	defer stop()
	r := tcpProbe(tcpExpectItem(addr, "SSH-2", 3000))
	if !r.OK || r.Cause != nil {
		t.Fatalf("straddling chunks: expected up, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

// Partial greeting then EOF, never matching => mismatch (we did get bytes), not no_banner.
func TestTCPProbe_PartialThenEOFIsMismatch(t *testing.T) {
	addr, stop := chunkServer(t, []string{"220-mail.example.com\r\n", "220 bye\r\n"}, 50*time.Millisecond, false)
	defer stop()
	r := tcpProbe(tcpExpectItem(addr, "+PONG", 3000))
	if r.OK || r.Cause == nil || *r.Cause != "tcp_mismatch" {
		t.Fatalf("partial then EOF: expected tcp_mismatch, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

// A non-matching banner on a connection the server keeps open must not hold a worker for the whole
// timeout: after the first bytes, a short idle window bounds the wait.
func TestTCPProbe_MismatchStopsAtIdleWindow(t *testing.T) {
	old := tcpBannerIdle
	tcpBannerIdle = 150 * time.Millisecond
	defer func() { tcpBannerIdle = old }()
	addr, stop := chunkServer(t, []string{"220 hello\r\n"}, 0, true)
	defer stop()
	start := time.Now()
	r := tcpProbe(tcpExpectItem(addr, "+PONG", 5000))
	if r.OK || r.Cause == nil || *r.Cause != "tcp_mismatch" {
		t.Fatalf("expected tcp_mismatch, got ok=%v cause=%v", r.OK, r.Cause)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("mismatch held the worker %v; idle window should have ended it", el)
	}
}

// A server that streams without end is capped at 4096 bytes (the expect is not within them).
func TestTCPProbe_ReadCappedAt4096(t *testing.T) {
	big := strings.Repeat("x", 4096)
	addr, stop := chunkServer(t, []string{big, "TAIL"}, 50*time.Millisecond, true)
	defer stop()
	r := tcpProbe(tcpExpectItem(addr, "TAIL", 3000))
	if r.OK || r.Cause == nil || *r.Cause != "tcp_mismatch" {
		t.Fatalf("expected tcp_mismatch past the 4 KB cap, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

// dripServer writes one byte every `every` until the client goes away (a slow-drip server).
func dripServer(t *testing.T, every time.Duration) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				for {
					if _, err := conn.Write([]byte("x")); err != nil {
						return
					}
					time.Sleep(every)
				}
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// Hardening (#389): one absolute deadline. A server dripping a byte every few ms (each arrival would
// otherwise keep an idle window alive) cannot hold the probe past the check timeout.
func TestTCPProbe_SlowDripEndsAtDeadline(t *testing.T) {
	addr, stop := dripServer(t, 10*time.Millisecond)
	defer stop()
	start := time.Now()
	r := tcpProbe(tcpExpectItem(addr, "NEVER", 700))
	el := time.Since(start)
	if r.OK || r.Cause == nil || *r.Cause != "tcp_mismatch" {
		t.Fatalf("slow drip: expected tcp_mismatch, got ok=%v cause=%v", r.OK, r.Cause)
	}
	if el < 600*time.Millisecond || el > 1500*time.Millisecond {
		t.Errorf("slow drip should end at the 700ms deadline, took %v", el)
	}
}

// The same under regex mode (the pattern must not be re-compiled or re-run unbounded per byte).
func TestTCPProbe_SlowDripRegexEndsAtDeadline(t *testing.T) {
	addr, stop := dripServer(t, 2*time.Millisecond)
	defer stop()
	item := tcpExpectItem(addr, "^NEVER$", 700)
	item.CheckSpec.Transport.ExpectMode = "regex"
	start := time.Now()
	r := tcpProbe(item)
	if r.OK || r.Cause == nil || *r.Cause != "tcp_mismatch" {
		t.Fatalf("regex drip: expected tcp_mismatch, got ok=%v cause=%v", r.OK, r.Cause)
	}
	if el := time.Since(start); el > 1500*time.Millisecond {
		t.Errorf("regex drip should end at the deadline, took %v", el)
	}
}

// A server flooding without pause stops the read at the byte cap, long before the timeout.
func TestTCPProbe_FloodStopsAtCap(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		chunk := []byte(strings.Repeat("y", 1024))
		for {
			if _, err := c.Write(chunk); err != nil {
				return
			}
		}
	}()
	start := time.Now()
	r := tcpProbe(tcpExpectItem(ln.Addr().String(), "NEVER", 8000))
	if r.OK || r.Cause == nil || *r.Cause != "tcp_mismatch" {
		t.Fatalf("flood: expected tcp_mismatch, got ok=%v cause=%v", r.OK, r.Cause)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Errorf("flood should stop at the 4096-byte cap, took %v", el)
	}
}

// newReplyMatcher must agree with matchReply for every expect mode (it is a compile-once copy of it).
func TestNewReplyMatcherMatchesMatchReply(t *testing.T) {
	cases := []struct {
		tr    Transport
		reply string
	}{
		{Transport{Expect: "220"}, "220 ready"},
		{Transport{Expect: "220"}, "421 busy"},
		{Transport{Expect: "^220 ", ExpectMode: "regex"}, "220-a\r\n220 b"},
		{Transport{Expect: "(?m)^220 ", ExpectMode: "regex"}, "220-a\r\n220 b"},
		{Transport{Expect: "aaaa8180", PayloadEncoding: "hex"}, "\xaa\xaa\x81\x80\x00"},
		{Transport{Expect: "aaaa8180", PayloadEncoding: "hex"}, "\xaa\xaa\x81\x05\x00"},
	}
	for _, c := range cases {
		m, err := newReplyMatcher(&c.tr)
		if err != nil {
			t.Fatalf("%+v: %v", c.tr, err)
		}
		want, _ := matchReply(&c.tr, []byte(c.reply))
		if got := m([]byte(c.reply)); got != want {
			t.Errorf("%+v on %q: newReplyMatcher=%v matchReply=%v", c.tr, c.reply, got, want)
		}
	}
	if _, err := newReplyMatcher(&Transport{Expect: "zz", PayloadEncoding: "hex"}); err == nil {
		t.Error("bad hex expect should error up front")
	}
	if _, err := newReplyMatcher(&Transport{Expect: "(", ExpectMode: "regex"}); err == nil {
		t.Error("bad regex should error up front")
	}
}
