package main

import (
	"errors"
	"net"
	"testing"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// nextICMPSeq must hand out distinct 16-bit sequence numbers so concurrent probes (sharing one pid
// as id) don't collide and read each other's echo replies.
func TestNextICMPSeqDistinct(t *testing.T) {
	a := nextICMPSeq()
	b := nextICMPSeq()
	if a == b {
		t.Fatalf("consecutive seqs must differ: %d == %d", a, b)
	}
	if a < 0 || a > 0xffff || b < 0 || b > 0xffff {
		t.Fatalf("seq out of 16-bit range: a=%d b=%d", a, b)
	}
}

// isOurEchoReply must also verify the reply came from the target IP. The raw ICMP socket receives
// every host's ICMP packets, so without a peer check one probe could match another probe's reply.
func TestIsOurEchoReplyChecksPeer(t *testing.T) {
	echo := &icmp.Echo{ID: 42, Seq: 7, Data: []byte("culipulse")}
	reply := &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Code: 0, Body: echo}
	target := net.ParseIP("192.0.2.10")
	peerMatch := &net.IPAddr{IP: net.ParseIP("192.0.2.10")}
	peerOther := &net.IPAddr{IP: net.ParseIP("192.0.2.99")}

	if !isOurEchoReply(reply, 42, 7, peerMatch, target) {
		t.Fatal("expected match for same id+seq from the target peer")
	}
	if isOurEchoReply(reply, 42, 7, peerOther, target) {
		t.Fatal("a reply from a different peer must not match (cross-probe guard)")
	}
	if isOurEchoReply(reply, 42, 8, peerMatch, target) {
		t.Fatal("a different seq must not match")
	}
	if isOurEchoReply(reply, 42, 7, nil, target) {
		t.Fatal("a nil peer must not match")
	}
}

func TestMatchesEchoReply(t *testing.T) {
	echo := &icmp.Echo{ID: 42, Seq: 1, Data: []byte("culipulse")}
	reply := &icmp.Message{Type: ipv4.ICMPTypeEchoReply, Code: 0, Body: echo}

	if !matchesEchoReply(reply, 42, 1) {
		t.Fatal("expected a match for the same id+seq")
	}
	if matchesEchoReply(reply, 99, 1) {
		t.Fatal("a different id must not match")
	}
	if matchesEchoReply(reply, 42, 2) {
		t.Fatal("a different seq must not match")
	}
	// an echo *request* (not a reply) must not match
	if matchesEchoReply(&icmp.Message{Type: ipv4.ICMPTypeEcho, Code: 0, Body: echo}, 42, 1) {
		t.Fatal("an echo request is not a reply")
	}
	// an echo-reply type with a non-Echo body must not match
	if matchesEchoReply(&icmp.Message{Type: ipv4.ICMPTypeEchoReply, Code: 0, Body: &icmp.DstUnreach{}}, 42, 1) {
		t.Fatal("a non-Echo body must not match")
	}
	if matchesEchoReply(nil, 42, 1) {
		t.Fatal("nil must not match")
	}
}

// A missing domain must be reported as dns_nxdomain (same classifier as http/tcp/udp), any other
// resolver failure as dns_error.
func TestICMPResolveCause(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nxdomain", &net.DNSError{Err: "no such host", IsNotFound: true}, "dns_nxdomain"},
		{"timeout", &net.DNSError{Err: "i/o timeout", IsTimeout: true}, "dns_error"},
		{"non-dns error", errors.New("weird"), "dns_error"},
	}
	for _, c := range cases {
		if got := icmpResolveCause(c.err); got != c.want {
			t.Errorf("%s: icmpResolveCause = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestICMPProbeUnresolvableDomainCause(t *testing.T) {
	res := icmpProbe(WorkItem{MonitorID: "m", Type: "icmp", Target: "nxdomain-test.invalid", TimeoutMs: 3000})
	if res.Cause == nil || (*res.Cause != "dns_nxdomain" && *res.Cause != "dns_error") {
		t.Fatalf("cause = %v, want dns_nxdomain or dns_error", res.Cause)
	}
}
