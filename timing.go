package main

import (
	"crypto/tls"
	"net/http/httptrace"
	"time"
)

// probeTiming records phase-boundary timestamps for one HTTP probe, captured via httptrace.
// A zero timestamp means the phase did not occur (e.g. a reused connection skips DNS/connect/TLS,
// a literal-IP target skips DNS). `done` is stamped by the prober after the body is drained.
type probeTiming struct {
	start        time.Time
	dnsStart     time.Time
	dnsDone      time.Time
	connectStart time.Time
	connectDone  time.Time
	tlsStart     time.Time
	tlsDone      time.Time
	wroteRequest time.Time
	firstByte    time.Time
	done         time.Time
	reused       bool
}

// newTimingTrace returns a ClientTrace that fills t. Install with httptrace.WithClientTrace.
func newTimingTrace(t *probeTiming) *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { t.dnsStart = time.Now() },
		DNSDone:  func(httptrace.DNSDoneInfo) { t.dnsDone = time.Now() },
		ConnectStart: func(_, _ string) {
			if t.connectStart.IsZero() {
				t.connectStart = time.Now()
			}
		},
		ConnectDone:          func(_, _ string, _ error) { t.connectDone = time.Now() },
		TLSHandshakeStart:    func() { t.tlsStart = time.Now() },
		TLSHandshakeDone:     func(tls.ConnectionState, error) { t.tlsDone = time.Now() },
		GotConn:              func(i httptrace.GotConnInfo) { t.reused = i.Reused },
		WroteRequest:         func(httptrace.WroteRequestInfo) { t.wroteRequest = time.Now() },
		GotFirstResponseByte: func() { t.firstByte = time.Now() },
	}
}

// toDetail derives the display-only timing object, omitting phases that did not occur. Returns nil
// if no response was received (no first byte) — there is nothing meaningful to show.
func (t *probeTiming) toDetail() map[string]any {
	if t.start.IsZero() || t.firstByte.IsZero() {
		return nil
	}
	ms := func(a, b time.Time) int64 {
		if a.IsZero() || b.IsZero() || b.Before(a) {
			return 0
		}
		return b.Sub(a).Milliseconds()
	}
	waitFrom := t.wroteRequest
	if waitFrom.IsZero() {
		waitFrom = t.firstByte // degenerate; wait ~0
	}
	end := t.done
	if end.IsZero() {
		end = t.firstByte
	}
	out := map[string]any{
		"connReused": t.reused,
		"waitMs":     ms(waitFrom, t.firstByte),
		"downloadMs": ms(t.firstByte, end),
		"totalMs":    ms(t.start, end),
	}
	if !t.dnsStart.IsZero() && !t.dnsDone.IsZero() {
		out["dnsMs"] = ms(t.dnsStart, t.dnsDone)
	}
	if !t.connectStart.IsZero() && !t.connectDone.IsZero() {
		out["connectMs"] = ms(t.connectStart, t.connectDone)
	}
	if !t.tlsStart.IsZero() && !t.tlsDone.IsZero() {
		out["tlsMs"] = ms(t.tlsStart, t.tlsDone)
	}
	return out
}
