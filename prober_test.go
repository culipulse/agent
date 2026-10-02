package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// A panicking probe must NOT crash the agent (which would halt all monitoring). It is recovered and
// turned into an abstain (a non-vote) — an agent bug is not evidence the target is down.
func TestSafeProbeRecoversFromPanic(t *testing.T) {
	item := WorkItem{MonitorID: "boom", Type: "http"}
	res := safeProbeFn(item, func(WorkItem) IngestResult { panic("kaboom") })
	if res.OK {
		t.Fatal("a panicking probe must not report ok")
	}
	if res.Cause == nil || *res.Cause != abstainCause {
		t.Fatalf("expected abstain cause %q, got %v", abstainCause, res.Cause)
	}
	if res.MonitorID != "boom" {
		t.Fatalf("result must carry the monitor id, got %q", res.MonitorID)
	}
	if res.TS == 0 {
		t.Fatal("result must carry a timestamp")
	}
}

// When the probe doesn't panic, safeProbeFn returns its result untouched.
func TestSafeProbeFnPassesThroughResult(t *testing.T) {
	item := WorkItem{MonitorID: "ok"}
	res := safeProbeFn(item, func(WorkItem) IngestResult { return IngestResult{MonitorID: "ok", OK: true} })
	if !res.OK || res.MonitorID != "ok" {
		t.Fatalf("expected pass-through, got %+v", res)
	}
}

// Two probes to the same host must reuse the underlying TCP connection. That requires both a shared
// transport (not a fresh one per probe) AND draining the response body before close so keep-alive
// can recycle the connection. The server observing the same RemoteAddr twice proves reuse.
func TestProbeReusesConnection(t *testing.T) {
	var mu sync.Mutex
	var remotes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remotes = append(remotes, r.RemoteAddr)
		mu.Unlock()
		_, _ = io.WriteString(w, "a body that must be drained for the connection to be reusable")
	}))
	defer srv.Close()

	item := WorkItem{MonitorID: "m", Target: srv.URL, ExpectedStatus: "2xx"}
	if res := probe(item); !res.OK {
		t.Fatalf("first probe not ok: %+v", res)
	}
	if res := probe(item); !res.OK {
		t.Fatalf("second probe not ok: %+v", res)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(remotes) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(remotes))
	}
	if remotes[0] != remotes[1] {
		t.Fatalf("expected connection reuse (same remote addr), got %q then %q", remotes[0], remotes[1])
	}
}

func TestProbeWith_CapturesTiming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	item := WorkItem{MonitorID: "m1", Type: "http", Target: srv.URL, Method: "GET", ExpectedStatus: "200", TimeoutMs: 5000}
	res := probeWith(buildClient(item), item)
	timing, ok := res.Detail["timing"].(map[string]any)
	if !ok {
		t.Fatalf("no timing in detail: %#v", res.Detail)
	}
	for _, k := range []string{"waitMs", "downloadMs", "totalMs", "connReused"} {
		if _, present := timing[k]; !present {
			t.Errorf("missing required timing key %s", k)
		}
	}
	if _, present := timing["tlsMs"]; present {
		t.Errorf("plain http target must omit tlsMs: %#v", timing)
	}
}

func TestProbeWith_ReusesConnection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	}))
	defer srv.Close()
	item := WorkItem{MonitorID: "m1", Type: "http", Target: srv.URL, Method: "GET", ExpectedStatus: "200", TimeoutMs: 5000}
	c := buildClient(item)
	_ = probeWith(c, item) // primes the pooled connection
	res := probeWith(c, item)
	timing := res.Detail["timing"].(map[string]any)
	if timing["connReused"] != true {
		t.Errorf("second probe should reuse the connection: %#v", timing)
	}
	if _, present := timing["connectMs"]; present {
		t.Errorf("reused connection must omit connectMs: %#v", timing)
	}
}

// End-to-end: probeWith captures the body + headers (duplicates joined "a, b" like the edge Fetch
// object) and classify evaluates content assertions — pass => UP, mismatch => DOWN.
func TestProbeWith_ContentAssertions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("X-Multi", "a")
		w.Header().Add("X-Multi", "b")
		io.WriteString(w, `{"status":"green"}`)
	}))
	defer srv.Close()
	base := WorkItem{MonitorID: "m1", Type: "http", Target: srv.URL, Method: "GET", ExpectedStatus: "200", TimeoutMs: 5000}

	pass := base
	pass.CheckSpec = CheckSpec{Assertions: []Assertion{
		{Source: "json_body", Op: "equals", Path: "$.status", Value: "green"},
		{Source: "header", Op: "equals", Name: "X-Multi", Value: "a, b"}, // duplicates joined
	}}
	if res := probeWith(buildClient(pass), pass); !res.OK || res.Degraded {
		t.Fatalf("content pass: ok=%v degraded=%v", res.OK, res.Degraded)
	}

	fail := base
	fail.CheckSpec = CheckSpec{Assertions: []Assertion{{Source: "text_body", Op: "contains", Value: "red"}}}
	if res := probeWith(buildClient(fail), fail); res.OK {
		t.Fatalf("content mismatch must be DOWN, got ok=true")
	}
}

func TestProbeHTTPS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	item := WorkItem{
		MonitorID:      "m",
		Target:         srv.URL,
		ExpectedStatus: "2xx",
		CheckSpec:      CheckSpec{Assertions: []Assertion{{Source: "cert_expiry", Op: "gt", Value: "100000"}}},
	}
	res := probeWith(srv.Client(), item)
	if res.HTTPStatus != 200 {
		t.Fatalf("status = %d, want 200", res.HTTPStatus)
	}
	if res.Detail == nil || res.Detail["daysRemaining"] == nil {
		t.Fatalf("expected cert detail, got %+v", res.Detail)
	}
	if !res.OK || !res.Degraded || res.Cause == nil || *res.Cause != "cert_expiring" {
		t.Errorf("expected up+degraded cert_expiring, got ok=%v deg=%v cause=%v", res.OK, res.Degraded, res.Cause)
	}

	item.CheckSpec.Assertions = []Assertion{{Source: "cert_expiry", Op: "gt", Value: "14"}}
	res2 := probeWith(srv.Client(), item)
	if !res2.OK || res2.Degraded {
		t.Errorf("expected healthy, got ok=%v deg=%v", res2.OK, res2.Degraded)
	}
}

// BUG C1: request body + content-type must reach the target server.
// Before the fix the Request struct had no Body/ContentType fields, so they were silently
// discarded at JSON unmarshal and probeWith always sent a nil body.
func TestProbeWith_ForwardsBodyAndContentType(t *testing.T) {
	var gotBody []byte
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	item := WorkItem{
		MonitorID:      "m",
		Type:           "http",
		Target:         srv.URL,
		Method:         "POST",
		ExpectedStatus: "200",
		TimeoutMs:      5000,
		CheckSpec: CheckSpec{
			Request: &Request{
				Body:        "hello=world",
				ContentType: "application/x-www-form-urlencoded",
			},
		},
	}
	res := probeWith(buildClient(item), item)
	if res.HTTPStatus != 200 {
		t.Fatalf("status = %d, want 200", res.HTTPStatus)
	}
	if string(gotBody) != "hello=world" {
		t.Errorf("body = %q, want %q", gotBody, "hello=world")
	}
	if gotCT != "application/x-www-form-urlencoded" {
		t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", gotCT)
	}
}

// Content-Type set explicitly in Headers must NOT be overwritten by CheckSpec.Request.ContentType.
func TestProbeWith_ExplicitHeaderContentTypeWins(t *testing.T) {
	var gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	item := WorkItem{
		MonitorID:      "m",
		Type:           "http",
		Target:         srv.URL,
		Method:         "POST",
		ExpectedStatus: "200",
		TimeoutMs:      5000,
		CheckSpec: CheckSpec{
			Request: &Request{
				Body:        "data",
				ContentType: "application/json",
				Headers:     map[string]string{"Content-Type": "text/plain"},
			},
		},
	}
	res := probeWith(buildClient(item), item)
	if res.HTTPStatus != 200 {
		t.Fatalf("status = %d, want 200", res.HTTPStatus)
	}
	if gotCT != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain (explicit header must win)", gotCT)
	}
}

func TestProbeDiagnose_FreshConnEachTime(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer srv.Close()
	item := WorkItem{MonitorID: "m1", Type: "http", Target: srv.URL, Method: "GET", ExpectedStatus: "200", TimeoutMs: 5000, RequestID: "req-123"}
	r1 := probeDiagnose(item)
	r2 := probeDiagnose(item)
	if r1.RequestID != "req-123" || r2.RequestID != "req-123" {
		t.Fatalf("RequestID not carried: %q %q", r1.RequestID, r2.RequestID)
	}
	// Fresh connection every time: neither probe reuses a pooled conn.
	for i, r := range []IngestResult{r1, r2} {
		tm, ok := r.Detail["timing"].(map[string]any)
		if !ok {
			t.Fatalf("probe %d missing timing", i)
		}
		if tm["connReused"] != false {
			t.Errorf("probe %d should NOT reuse a connection (fresh-conn diagnose)", i)
		}
		if _, ok := tm["connectMs"]; !ok {
			t.Errorf("probe %d fresh conn should measure connectMs", i)
		}
	}
}

// #334: a domain that doesn't resolve must surface as a DNS cause, not "conn_error".
// Classification is by error IDENTITY (*net.DNSError), never by error text.
func TestClassifyDNSError(t *testing.T) {
	nx := &net.DNSError{Err: "no such host", Name: "nxdomain-test.invalid", IsNotFound: true}
	to := &net.DNSError{Err: "i/o timeout", Name: "slow.example", IsTimeout: true}
	servfail := &net.DNSError{Err: "server misbehaving", Name: "x.example", IsTemporary: true}
	wrap := func(e error) error { return &net.OpError{Op: "dial", Net: "tcp", Err: e} }
	cases := []struct {
		name   string
		err    error
		want   string
		wantOK bool
	}{
		{"nxdomain", wrap(nx), "dns_nxdomain", true},
		{"timeout", wrap(to), "dns_error", true},
		{"servfail", wrap(servfail), "dns_error", true},
		{"refused is not dns", wrap(errors.New("connect: connection refused")), "", false},
		{"text alone is not dns", errors.New("lookup x: no such host"), "", false},
	}
	for _, c := range cases {
		got, ok := classifyDNSError(c.err)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: classifyDNSError = (%q,%v), want (%q,%v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

func TestClassifyDNSError_HTTPWrapped(t *testing.T) {
	// net/http wraps dial failures in *url.Error; its text contains "timeout" for a DNS timeout,
	// which the old switch mapped to "timeout". The DNS check must win.
	mk := func(d *net.DNSError) error {
		return &url.Error{Op: "Get", URL: "https://x.example/", Err: &net.OpError{Op: "dial", Net: "tcp", Err: d}}
	}
	if got := classifyError(mk(&net.DNSError{Err: "no such host", Name: "x.example", IsNotFound: true})); got != "dns_nxdomain" {
		t.Errorf("http nxdomain: got %q, want dns_nxdomain", got)
	}
	if got := classifyError(mk(&net.DNSError{Err: "i/o timeout", Name: "x.example", IsTimeout: true})); got != "dns_error" {
		t.Errorf("http dns timeout: got %q, want dns_error", got)
	}
	if got := classifyTCPError(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "i/o timeout", IsTimeout: true}}); got != "dns_error" {
		t.Errorf("tcp dns timeout: got %q, want dns_error (was tcp_timeout)", got)
	}
	if got := classifyTCPError(&net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}); got != "dns_nxdomain" {
		t.Errorf("tcp nxdomain: got %q, want dns_nxdomain", got)
	}
	if got := classifyUDPError(&net.OpError{Op: "dial", Net: "udp", Err: &net.DNSError{Err: "i/o timeout", IsTimeout: true}}); got != "dns_error" {
		t.Errorf("udp dns timeout: got %q, want dns_error (was udp_no_reply)", got)
	}
	if got := classifyUDPError(&net.OpError{Op: "dial", Net: "udp", Err: &net.DNSError{Err: "no such host", IsNotFound: true}}); got != "dns_nxdomain" {
		t.Errorf("udp nxdomain: got %q, want dns_nxdomain", got)
	}
}

// Untyped errors that merely carry lookup text keep the TCP/UDP string fallback (unchanged behaviour).
func TestClassifyDNSError_TCPUDPStringFallbackKept(t *testing.T) {
	e := errors.New("lookup x.example: no such host")
	if got := classifyTCPError(e); got != "dns_error" {
		t.Errorf("tcp fallback: got %q, want dns_error", got)
	}
	if got := classifyUDPError(e); got != "dns_error" {
		t.Errorf("udp fallback: got %q, want dns_error", got)
	}
}

// The HTTP classifier must not gain a DNS string match: a refused connection to a URL whose path
// contains DNS-ish text stays conn_error (spoof rule, cf. TestClassifyError_NoSpoofFromTargetURLText).
func TestClassifyError_NoDNSSpoofFromTargetURLText(t *testing.T) {
	res := probe(WorkItem{MonitorID: "m", Type: "http", Target: "http://127.0.0.1:1/no%20such%20host-lookup", TimeoutMs: 1000, Method: "GET", ExpectedStatus: "2xx"})
	if res.Cause == nil || *res.Cause != "conn_error" {
		t.Fatalf("want conn_error, got cause=%v", res.Cause)
	}
}

// End-to-end through the real resolver and http.Client wrap chain. ".invalid" never resolves
// (RFC 6761); depending on sandbox DNS the lookup is NXDOMAIN or a resolver error — either way a
// DNS cause, never conn_error.
func TestProbe_UnresolvableHostIsDNSCause(t *testing.T) {
	res := probe(WorkItem{MonitorID: "m", Type: "http", Target: "http://nxdomain-test.invalid/", TimeoutMs: 3000, Method: "GET", ExpectedStatus: "2xx"})
	if res.Cause == nil || (*res.Cause != "dns_nxdomain" && *res.Cause != "dns_error") {
		t.Fatalf("want dns_nxdomain or dns_error, got ok=%v cause=%v", res.OK, res.Cause)
	}
}
