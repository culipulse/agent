package main

import (
	"encoding/hex"
	"net"
	"testing"
)

// udpEcho starts a localhost UDP responder that replies with `reply` to any datagram.
// Returns its address and a stop func. If reply is nil, it reads but never responds (silent).
func udpEcho(t *testing.T, reply []byte) (string, func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return // socket closed by stop()
			}
			if reply != nil {
				_, _ = pc.WriteTo(reply, addr)
			} else {
				_ = n
			}
		}
	}()
	return pc.LocalAddr().String(), func() { pc.Close() }
}

func TestUDPProbe_AnyReply(t *testing.T) {
	addr, stop := udpEcho(t, []byte("pong"))
	defer stop()
	// No payload/expect: any reply ⇒ up.
	r := udpProbe(WorkItem{MonitorID: "m", Type: "udp", Target: addr, TimeoutMs: 1000})
	if !r.OK || r.Cause != nil {
		t.Errorf("any-reply: expected up, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

func TestUDPProbe_PayloadExpectMatch(t *testing.T) {
	addr, stop := udpEcho(t, []byte("HTTP/1.1 200 healthy"))
	defer stop()
	cs := CheckSpec{Transport: &Transport{Payload: "ping", Expect: "healthy"}}
	r := udpProbe(WorkItem{MonitorID: "m", Type: "udp", Target: addr, TimeoutMs: 1000, CheckSpec: cs})
	if !r.OK || r.Cause != nil {
		t.Errorf("expect match: expected up, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

func TestUDPProbe_PayloadExpectMismatch(t *testing.T) {
	addr, stop := udpEcho(t, []byte("nope"))
	defer stop()
	cs := CheckSpec{Transport: &Transport{Payload: "ping", Expect: "healthy"}}
	r := udpProbe(WorkItem{MonitorID: "m", Type: "udp", Target: addr, TimeoutMs: 1000, CheckSpec: cs})
	if r.OK || r.Cause == nil || *r.Cause != "udp_mismatch" {
		t.Errorf("mismatch: expected down udp_mismatch, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

func TestUDPProbe_NoReply(t *testing.T) {
	addr, stop := udpEcho(t, nil) // silent responder
	defer stop()
	r := udpProbe(WorkItem{MonitorID: "m", Type: "udp", Target: addr, TimeoutMs: 400})
	if r.OK || r.Cause == nil || *r.Cause != "udp_no_reply" {
		t.Errorf("silence: expected down udp_no_reply, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

func TestUDPProbe_HexEncoding(t *testing.T) {
	// reply contains the bytes 0xDEADBEEF; expect them as hex.
	reply, _ := hex.DecodeString("00deadbeef00")
	addr, stop := udpEcho(t, reply)
	defer stop()
	cs := CheckSpec{Transport: &Transport{Payload: "abcd", PayloadEncoding: "hex", Expect: "deadbeef"}}
	r := udpProbe(WorkItem{MonitorID: "m", Type: "udp", Target: addr, TimeoutMs: 1000, CheckSpec: cs})
	if !r.OK || r.Cause != nil {
		t.Errorf("hex expect: expected up, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

func TestUDPProbe_RegexMatch(t *testing.T) {
	addr, stop := udpEcho(t, []byte("status: healthy (uptime 1234)"))
	defer stop()
	cs := CheckSpec{Transport: &Transport{Payload: "ping", Expect: "^status: (ok|healthy)", ExpectMode: "regex"}}
	r := udpProbe(WorkItem{MonitorID: "m", Type: "udp", Target: addr, TimeoutMs: 1000, CheckSpec: cs})
	if !r.OK || r.Cause != nil {
		t.Errorf("regex match: expected up, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

func TestUDPProbe_RegexNoMatch(t *testing.T) {
	addr, stop := udpEcho(t, []byte("status: degraded"))
	defer stop()
	cs := CheckSpec{Transport: &Transport{Payload: "ping", Expect: "^status: (ok|healthy)", ExpectMode: "regex"}}
	r := udpProbe(WorkItem{MonitorID: "m", Type: "udp", Target: addr, TimeoutMs: 1000, CheckSpec: cs})
	if r.OK || r.Cause == nil || *r.Cause != "udp_mismatch" {
		t.Errorf("regex no-match: expected down udp_mismatch, got ok=%v cause=%v", r.OK, r.Cause)
	}
}

func TestUDPProbe_DispatchAndUnknownHost(t *testing.T) {
	r := probe(WorkItem{MonitorID: "m", Type: "udp", Target: "no.such.host.invalid:53", TimeoutMs: 500})
	if r.OK {
		t.Errorf("udp to bogus host should be down")
	}
}
