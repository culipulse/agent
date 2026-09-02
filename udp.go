package main

import (
	"net"
	"strings"
	"time"
)

// udpProbe runs a single UDP check. UDP is connectionless, so "up" is defined explicitly:
//   - with a payload: send it; up = a reply arrives within the timeout (matching Expect if set).
//   - without a payload: send an empty datagram; up = any reply within the timeout.
//
// Silence is always "down" (udp_no_reply) — we never treat it as up, which would require raw-ICMP
// port-unreachable detection. Operators should supply a protocol-correct payload for a meaningful
// check. Mirrors the cause vocabulary of the other probers (dns_error on lookup failure).
func udpProbe(item WorkItem) IngestResult {
	res := IngestResult{MonitorID: item.MonitorID, TS: time.Now().Unix()}
	timeout := time.Duration(item.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	payload, err := decodePayload(item.CheckSpec.Transport)
	if err != nil {
		c := "udp_error"
		res.Cause = &c
		return res
	}

	start := time.Now()
	conn, err := net.DialTimeout("udp", item.Target, timeout)
	if err != nil {
		c := classifyUDPError(err)
		res.Cause = &c
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(payload); err != nil {
		c := classifyUDPError(err)
		res.Cause = &c
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}

	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		c := classifyUDPError(err)
		res.Cause = &c
		return res
	}
	reply := buf[:n]

	matched, merr := matchReply(item.CheckSpec.Transport, reply)
	if merr != nil {
		c := "udp_error"
		res.Cause = &c
		return res
	}
	if !matched {
		c := "udp_mismatch"
		res.Cause = &c
		return res
	}
	res.OK = true
	return res
}

func classifyUDPError(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline") || strings.Contains(s, "i/o timeout"):
		return "udp_no_reply"
	case strings.Contains(s, "refused"):
		// An ICMP port-unreachable surfaces as ECONNREFUSED on a connected UDP socket ⇒ closed port.
		return "udp_no_reply"
	case strings.Contains(s, "no such host") || strings.Contains(s, "lookup"):
		return "dns_error"
	default:
		return "udp_error"
	}
}
