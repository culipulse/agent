package main

import (
	"net"
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
