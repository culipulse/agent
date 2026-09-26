package main

import (
	"errors"
	"strings"
	"time"
)

func tcpProbe(item WorkItem) IngestResult {
	res := IngestResult{MonitorID: item.MonitorID, TS: time.Now().Unix()}
	timeout := time.Duration(item.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	start := time.Now()
	conn, err := guardedDialer(timeout).Dial("tcp", item.Target)
	if err != nil {
		c := classifyTCPError(err)
		res.Cause = &c
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	defer conn.Close()

	t := item.CheckSpec.Transport
	// Plain connect check: no transport (or nothing to send/expect) ⇒ up on connect, as before.
	if t == nil || (t.Payload == "" && t.Expect == "") {
		res.LatencyMs = time.Since(start).Milliseconds()
		res.OK = true
		return res
	}

	payload, derr := decodePayload(t)
	if derr != nil {
		c := "tcp_error"
		res.Cause = &c
		res.LatencyMs = time.Since(start).Milliseconds()
		return res
	}
	_ = conn.SetDeadline(deadline)
	if len(payload) > 0 {
		if _, werr := conn.Write(payload); werr != nil {
			c := classifyTCPError(werr)
			res.Cause = &c
			res.LatencyMs = time.Since(start).Milliseconds()
			return res
		}
	}
	// No expect ⇒ an open connection (and the optional write) is proof enough; don't block on a reply
	// a protocol may never send unprompted.
	if t.Expect == "" {
		res.LatencyMs = time.Since(start).Milliseconds()
		res.OK = true
		return res
	}

	// Expect set ⇒ read a banner and match. Silence/EOF before the deadline ⇒ tcp_no_banner.
	buf := make([]byte, 4096)
	n, rerr := conn.Read(buf)
	res.LatencyMs = time.Since(start).Milliseconds()
	// A server may send its banner and close in one segment ⇒ (n>0, io.EOF). Evaluate the bytes we
	// got before treating the connection as silent, so edge and agent agree on banner-then-close.
	if n > 0 {
		matched, merr := matchReply(t, buf[:n])
		if merr != nil {
			c := "tcp_error"
			res.Cause = &c
			return res
		}
		if !matched {
			c := "tcp_mismatch"
			res.Cause = &c
			return res
		}
		res.OK = true
		return res
	}
	// No data (silence / EOF before any byte / read timeout) ⇒ no banner.
	// Note: rerr may be io.EOF (expected when server closes) or a timeout (read deadline exceeded).
	_ = rerr
	cnb := "tcp_no_banner"
	res.Cause = &cnb
	return res
}

func classifyTCPError(err error) string {
	// Identity check first — never string-match, which a target string containing our cause text
	// could spoof (see classifyError in prober.go for the concrete spoof example).
	if errors.Is(err, errBlockedTarget) {
		return "blocked_target"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "refused"):
		return "tcp_refused"
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline"):
		return "tcp_timeout"
	case strings.Contains(s, "no such host") || strings.Contains(s, "lookup"):
		return "dns_error"
	default:
		return "tcp_error"
	}
}
