package main

import (
	"errors"
	"strings"
	"time"
)

// tcpBannerMax caps how much of a greeting we accumulate while looking for the expected text.
const tcpBannerMax = 4096

// tcpBannerIdle bounds the wait for MORE bytes once some have arrived, so a banner that never matches
// does not hold one of the shared agent's workers for the whole timeout. Silence before the first byte
// still waits the full timeout (a slow server may greet late). A var so tests can shrink it.
//
// 3 s, not shorter: some servers pause between the lines of a multi-line greeting (Postfix postscreen
// sends "220-" and then holds the real "220 " line back while it checks the client), and a 1.5 s window
// cut common pauses off as a false mismatch. 3 s is a trade-off, not a guarantee: a server that pauses
// longer (postscreen's greet wait can reach ~6 s) is still judged on what had arrived, which the docs say.
// The cost is bounded: a non-matching banner holds a worker at most this long past its last byte, and
// never past the overall timeout deadline.
var tcpBannerIdle = 3 * time.Second

// newReplyMatcher is matchReply with the regex compiled once, so matching the growing buffer after
// each read doesn't recompile the pattern. The buffer is already bounded by tcpBannerMax, and Go's
// regexp is linear-time (RE2), so re-matching ≤4 KB per read stays cheap. A bad pattern errors up front.
// It MUST stay in step with matchReply in transport.go (same expect semantics: regex via expectRegex,
// hex-decoded or plain substring otherwise); the two are cross-checked by TestNewReplyMatcherMatchesMatchReply.
func newReplyMatcher(t *Transport) (func([]byte) bool, error) {
	if strings.ToLower(t.ExpectMode) == "regex" {
		re, err := expectRegex(t)
		if err != nil {
			return nil, err
		}
		return re.Match, nil
	}
	if _, err := matchReply(t, nil); err != nil { // bad hex expect ⇒ error up front, as before
		return nil, err
	}
	return func(b []byte) bool {
		ok, err := matchReply(t, b)
		return err == nil && ok
	}, nil
}

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

	// Expect set ⇒ read a banner and match. Greetings can span several TCP segments (multi-line SMTP
	// "220-" then "220 "), so keep reading and match against everything accumulated, until: it matches,
	// the server closes, tcpBannerMax bytes are held, or the wait runs out (the overall deadline, or
	// tcpBannerIdle once some bytes have arrived). Silence/EOF before any byte ⇒ tcp_no_banner.
	// `deadline` is fixed once from the check timeout and never extended: each read below may only
	// shorten it (idle window), so a slow-drip server cannot keep the probe alive past the timeout.
	match, merr := newReplyMatcher(t)
	if merr != nil {
		res.LatencyMs = time.Since(start).Milliseconds()
		c := "tcp_error"
		res.Cause = &c
		return res
	}
	buf := make([]byte, tcpBannerMax) // total cap across ALL reads (buf[got:] never exceeds it)
	got := 0
	for got < len(buf) {
		if got > 0 {
			rd := time.Now().Add(tcpBannerIdle)
			if rd.After(deadline) {
				rd = deadline
			}
			_ = conn.SetReadDeadline(rd)
		}
		n, rerr := conn.Read(buf[got:])
		// A server may send its banner and close in one segment ⇒ (n>0, io.EOF). Evaluate the bytes we
		// got before treating the connection as finished, so edge and agent agree on banner-then-close.
		got += n
		if n > 0 {
			if match(buf[:got]) {
				res.LatencyMs = time.Since(start).Milliseconds()
				res.OK = true
				return res
			}
		}
		if rerr != nil {
			break // EOF, or a read timeout (deadline / idle window)
		}
	}
	res.LatencyMs = time.Since(start).Milliseconds()
	if got > 0 {
		c := "tcp_mismatch"
		res.Cause = &c
		return res
	}
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
	if c, ok := classifyDNSError(err); ok {
		return c
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
