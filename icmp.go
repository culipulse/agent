package main

import (
	"net"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

// icmpSeqCounter hands out per-probe sequence numbers. All probes in one process share the same ICMP
// id (the pid), so the seq is what disambiguates concurrent in-flight echoes from each other.
var icmpSeqCounter uint32

// nextICMPSeq returns a distinct 16-bit ICMP sequence number for an in-flight probe. Concurrent
// probes MUST NOT share a (id, seq): each opens its own raw socket but every socket receives all of
// the host's ICMP traffic, so a shared seq lets one probe match another probe's reply (false up).
func nextICMPSeq() int {
	return int(atomic.AddUint32(&icmpSeqCounter, 1) & 0xffff)
}

// matchesEchoReply reports whether rm is an ICMP echo reply to OUR request (matching id+seq).
// The raw "ip4:icmp" socket receives every ICMP packet the host gets, so we must filter to our
// own echo — otherwise a stray reply (another process's ping) could be mistaken for ours.
func matchesEchoReply(rm *icmp.Message, id, seq int) bool {
	if rm == nil || rm.Type != ipv4.ICMPTypeEchoReply {
		return false
	}
	echo, ok := rm.Body.(*icmp.Echo)
	return ok && echo.ID == id && echo.Seq == seq
}

// isOurEchoReply is matchesEchoReply plus a peer check: the reply must come from the target IP.
// Because the raw socket sees every host's ICMP, the peer check is what stops a concurrent probe to
// host A from accepting host B's reply (which can share our pid-derived id if seq ever collides).
func isOurEchoReply(rm *icmp.Message, id, seq int, peer net.Addr, target net.IP) bool {
	if !matchesEchoReply(rm, id, seq) {
		return false
	}
	pa, ok := peer.(*net.IPAddr)
	if !ok || pa == nil {
		return false
	}
	return pa.IP.Equal(target)
}

func icmpProbe(item WorkItem) IngestResult {
	res := IngestResult{MonitorID: item.MonitorID, TS: time.Now().Unix()}
	timeout := time.Duration(item.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ipaddr, err := net.ResolveIPAddr("ip4", item.Target)
	if err != nil {
		c := "dns_error"
		res.Cause = &c
		return res
	}
	if sharedGuard.Load() && ipBlockedForShared(ipaddr.IP) {
		c := "blocked_target"
		res.Cause = &c
		return res
	}
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		c := "icmp_socket"
		res.Cause = &c
		return res
	}
	defer conn.Close()

	id := os.Getpid() & 0xffff
	count := pingCount(item.CheckSpec.Ping)
	// Split the timeout budget across the N echoes so the whole check still fits within timeout_ms.
	per := timeout / time.Duration(count)
	if per <= 0 {
		per = timeout
	}

	rtts := make([]time.Duration, 0, count)
	for i := 0; i < count; i++ {
		rtt, ok := pingOnce(conn, ipaddr, id, per)
		if ok {
			rtts = append(rtts, rtt)
		}
	}

	st := computePingStats(count, rtts)
	res.LatencyMs = int64(st.RttAvgMs) // avg over received; 0 when total loss
	res.Detail = map[string]any{"ping": map[string]any{
		"sent": st.Sent, "received": st.Received, "lossPct": st.LossPct,
		"rttMinMs": st.RttMinMs, "rttAvgMs": st.RttAvgMs, "rttMaxMs": st.RttMaxMs, "rttStddevMs": st.RttStddevMs,
	}}
	if st.Received == 0 { // total silence = down, same trigger as before
		c := "icmp_timeout"
		res.Cause = &c
		return res
	}
	res.OK = true
	if pingDegraded(st, item.CheckSpec.Ping) {
		res.Degraded = true
		c := "slow"
		res.Cause = &c
	}
	return res
}

// pingOnce sends one echo (distinct seq) and waits up to `per` for OUR reply. Returns the RTT and true
// on a matching reply, or false on timeout / any send/build/read error (the packet counts as lost — a
// transient per-packet failure must not abort the whole multi-packet check).
func pingOnce(conn *icmp.PacketConn, ipaddr *net.IPAddr, id int, per time.Duration) (time.Duration, bool) {
	seq := nextICMPSeq()
	msg := icmp.Message{
		Type: ipv4.ICMPTypeEcho, Code: 0,
		Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("culipulse")},
	}
	wb, err := msg.Marshal(nil)
	if err != nil {
		return 0, false
	}
	start := time.Now()
	if _, err := conn.WriteTo(wb, ipaddr); err != nil {
		return 0, false
	}
	_ = conn.SetReadDeadline(start.Add(per))
	rb := make([]byte, 1500)
	for {
		n, peer, err := conn.ReadFrom(rb)
		if err != nil { // deadline ⇒ this echo is lost
			return 0, false
		}
		rm, err := icmp.ParseMessage(1, rb[:n])
		if err != nil {
			continue
		}
		if isOurEchoReply(rm, id, seq, peer, ipaddr.IP) {
			return time.Since(start), true
		}
		// not ours — keep reading until our reply or the deadline
	}
}
