package main

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"
)

// hasContentAssertions reports whether any assertion needs the response body/headers captured
// (text_body/json_body/header). When false we just drain the body for connection reuse.
func hasContentAssertions(as []Assertion) bool {
	for _, a := range as {
		if a.Source == "text_body" || a.Source == "json_body" || a.Source == "header" {
			return true
		}
	}
	return false
}

// bodyReader returns an io.Reader for the request body, or nil when there is no body.
func bodyReader(spec *Request) io.Reader {
	if spec != nil && spec.Body != "" {
		return strings.NewReader(spec.Body)
	}
	return nil
}

// safeProbe runs probe under panic recovery. A panic in one probe must never crash the agent — that
// would halt monitoring for EVERY monitor it serves. A recovered probe abstains (a non-vote), since
// an agent bug is not evidence about the target; the panic is logged for visibility.
func safeProbe(item WorkItem) IngestResult {
	return safeProbeFn(item, probe)
}

func safeProbeFn(item WorkItem, fn func(WorkItem) IngestResult) (res IngestResult) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("probe panic monitor=%s type=%s: %v", item.MonitorID, item.Type, r)
			res = abstainResult(item)
		}
	}()
	return fn(item)
}

const agentUA = "Mozilla/5.0 (compatible; culipulse-agent/0.1)"

// maxDrainBytes caps how much of a probe response body we read back before closing. Draining lets
// the keep-alive connection be reused; capping it avoids pulling a huge body into io.Discard just to
// recycle a socket. Bodies larger than this simply don't get their connection reused (as before).
const maxDrainBytes = 64 << 10

// sharedTransport is reused across all HTTP probes so connections are pooled and TLS handshakes are
// amortized instead of paying a full handshake per probe (a fresh Transport per probe pooled nothing).
// Per-probe behavior that can't be shared (redirect policy, timeout) lives on a cheap per-probe
// *http.Client / request context that wraps this transport.
var sharedTransport = &http.Transport{
	MaxIdleConns:          200,
	MaxIdleConnsPerHost:   4,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
	TLSClientConfig:       &tls.Config{},
	DialContext:           guardedDialer(30 * time.Second).DialContext,
}

func buildClient(item WorkItem) *http.Client {
	// No Client.Timeout: the per-request context (probeWith) governs the deadline, so this client is
	// just a thin wrapper carrying the per-item redirect policy over the shared connection pool.
	return &http.Client{
		Transport:     sharedTransport,
		CheckRedirect: redirectPolicy(item),
	}
}

// buildFreshClient returns a client whose transport disables keep-alives, so every diagnose probe
// pays a full cold-start handshake — the whole point of on-demand diagnosis vs. the passive (possibly
// connection-reused) snapshot. A per-call transport is fine: diagnose is rare and explicitly one-shot.
func buildFreshClient(item WorkItem) *http.Client {
	tr := &http.Transport{
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		TLSClientConfig:     &tls.Config{},
		DialContext:         guardedDialer(30 * time.Second).DialContext,
	}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: redirectPolicy(item),
	}
}

// drainAndClose reads (and discards) up to maxDrainBytes of the body before closing, so the
// underlying keep-alive connection can be returned to the pool and reused by a later probe.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.CopyN(io.Discard, body, maxDrainBytes)
	_ = body.Close()
}

func probe(item WorkItem) IngestResult {
	switch item.Type {
	case "tcp":
		return tcpProbe(item)
	case "icmp":
		return icmpProbe(item)
	case "udp":
		return udpProbe(item)
	default:
		return probeWith(buildClient(item), item)
	}
}

func probeWith(client *http.Client, item WorkItem) IngestResult {
	res := IngestResult{MonitorID: item.MonitorID, TS: time.Now().Unix()}
	method := item.Method
	if method == "" {
		method = "GET"
	}
	timeout := time.Duration(item.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, item.Target, bodyReader(item.CheckSpec.Request))
	if err != nil {
		c := "bad_request"
		res.Cause = &c
		return res
	}
	req.Header.Set("User-Agent", agentUA)
	applyRequestSpec(req, item.CheckSpec.Request)

	start := time.Now()
	timing := probeTiming{start: start}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), newTimingTrace(&timing)))
	resp, err := client.Do(req)
	res.LatencyMs = time.Since(start).Milliseconds()
	if err != nil {
		c := classifyError(err)
		res.Cause = &c
		return res
	}
	res.HTTPStatus = resp.StatusCode

	var days *float64
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		cert := resp.TLS.PeerCertificates[0]
		d := time.Until(cert.NotAfter).Hours() / 24
		days = &d
		res.Detail = map[string]any{"daysRemaining": int(d), "notAfter": cert.NotAfter.UTC().Format(time.RFC3339)}
	}

	// Capture the body + headers only when a content assertion needs them (bounded to 1 MiB, matching
	// the edge in run-probe.ts); otherwise just drain for keep-alive reuse. Then stamp the download end.
	var body string
	var headers map[string]string
	if hasContentAssertions(item.CheckSpec.Assertions) {
		const maxBody = 1 << 20                                // 1 MiB — matches the edge cap (bytes here vs UTF-16 code units there;
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody)) // only diverges for content right at the boundary)
		body = string(b)
		headers = make(map[string]string, len(resp.Header))
		for k, vs := range resp.Header {
			if len(vs) > 0 {
				// Join duplicates with ", " to match the Fetch Headers object the edge reads.
				headers[strings.ToLower(k)] = strings.Join(vs, ", ")
			}
		}
		_ = resp.Body.Close()
	} else {
		drainAndClose(resp.Body)
	}
	timing.done = time.Now()
	if td := timing.toDetail(); td != nil {
		if res.Detail == nil {
			res.Detail = map[string]any{}
		}
		res.Detail["timing"] = td
	}

	statusOK := statusMatches(resp.StatusCode, item.ExpectedStatus)
	ok, degraded, cause := classify(statusOK, resp.StatusCode, res.LatencyMs, days, item.CheckSpec.Assertions, body, headers)
	res.OK = ok
	res.Degraded = degraded
	if cause != "" {
		res.Cause = &cause
	}
	return res
}

func classifyError(err error) string {
	// Identity check first: a target URL/host that merely CONTAINS the text "blocked_target" must
	// never spoof this cause. errors.Is walks the wrap chain (url.Error -> net.OpError -> our
	// sentinel), which string-matching the error text cannot distinguish from user-controlled text.
	if errors.Is(err, errBlockedTarget) {
		return "blocked_target"
	}
	if errors.Is(err, errInsecureRedirect) {
		return "insecure_redirect"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "certificate has expired") || strings.Contains(s, "certificate is not yet valid"):
		return "cert_expired"
	case strings.Contains(s, "x509") || strings.Contains(s, "tls:"):
		return "tls_error"
	case strings.Contains(s, "deadline exceeded") || strings.Contains(s, "Timeout") || strings.Contains(s, "timeout"):
		return "timeout"
	default:
		return "conn_error"
	}
}

// probeDiagnose runs a one-shot diagnostic probe on a fresh connection and tags the result with the
// request id so the worker can correlate it. Panic-safe like safeProbe.
func probeDiagnose(item WorkItem) IngestResult {
	res := safeProbeFn(item, func(it WorkItem) IngestResult { return probeWith(buildFreshClient(it), it) })
	res.RequestID = item.RequestID
	return res
}
