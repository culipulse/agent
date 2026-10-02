package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func mustReq(t *testing.T, method, raw string) *http.Request {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Request{Method: method, URL: u, Header: http.Header{}}
}

func secretItem() WorkItem {
	return WorkItem{FollowRedirects: true, CheckSpec: CheckSpec{Request: &Request{
		Headers:           map[string]string{"X-Api-Key": "sek", "Accept": "application/json"},
		SecretHeaderNames: []string{"X-Api-Key"},
	}}}
}

// hop builds the next redirect request the way net/http does before calling CheckRedirect:
// headers copied from the original request (Go itself already drops Authorization off-domain).
func hop(t *testing.T, method, raw string) *http.Request {
	r := mustReq(t, method, raw)
	r.Header.Set("X-Api-Key", "sek")
	r.Header.Set("Accept", "application/json")
	return r
}

func TestRedirectPolicy_FollowRedirectsOffAndHopCap(t *testing.T) {
	it := secretItem()
	it.FollowRedirects = false
	via := []*http.Request{mustReq(t, "GET", "https://a.example/")}
	if err := redirectPolicy(it)(hop(t, "GET", "https://a.example/x"), via); err != http.ErrUseLastResponse {
		t.Fatalf("follow off: want ErrUseLastResponse, got %v", err)
	}
	it.FollowRedirects = true
	ten := make([]*http.Request, 10)
	for i := range ten {
		ten[i] = mustReq(t, "GET", "https://a.example/")
	}
	if err := redirectPolicy(it)(hop(t, "GET", "https://a.example/x"), ten); err != http.ErrUseLastResponse {
		t.Fatalf("10 hops: want ErrUseLastResponse, got %v", err)
	}
}

func TestRedirectPolicy_SameHostKeepsSecrets(t *testing.T) {
	via := []*http.Request{mustReq(t, "GET", "https://api.example.com/")}
	req := hop(t, "GET", "https://API.example.com:8443/v2")
	if err := redirectPolicy(secretItem())(req, via); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("X-Api-Key") != "sek" {
		t.Fatal("same host (case/port differ) must keep the secret header")
	}
}

func TestRedirectPolicy_SubdomainKeepsSecrets(t *testing.T) {
	via := []*http.Request{mustReq(t, "GET", "https://example.com/")}
	req := hop(t, "GET", "https://www.example.com/")
	if err := redirectPolicy(secretItem())(req, via); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("X-Api-Key") != "sek" {
		t.Fatal("example.com → www.example.com must keep the secret header")
	}
}

func TestRedirectPolicy_OtherHostStripsSecretsKeepsPlain(t *testing.T) {
	via := []*http.Request{mustReq(t, "GET", "https://example.com/")}
	req := hop(t, "GET", "https://evil.test/")
	if err := redirectPolicy(secretItem())(req, via); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("X-Api-Key") != "" {
		t.Fatal("secret header must be withheld from another host")
	}
	if req.Header.Get("Accept") != "application/json" {
		t.Fatal("a plain header is not secret and must still be sent")
	}
	// A look-alike suffix is not a subdomain.
	req2 := hop(t, "GET", "https://notexample.com/")
	_ = redirectPolicy(secretItem())(req2, via)
	if req2.Header.Get("X-Api-Key") != "" {
		t.Fatal("notexample.com is not a subdomain of example.com")
	}
}

func TestRedirectPolicy_StaysStrippedAfterLeavingHost(t *testing.T) {
	via := []*http.Request{mustReq(t, "GET", "https://example.com/"), mustReq(t, "GET", "https://evil.test/")}
	req := hop(t, "GET", "https://example.com/back")
	_ = redirectPolicy(secretItem())(req, via)
	if req.Header.Get("X-Api-Key") != "" {
		t.Fatal("once the chain left the host, secrets stay withheld even when it comes back")
	}
}

func TestRedirectPolicy_StripsAuthAndSecretBody(t *testing.T) {
	it := WorkItem{FollowRedirects: true, CheckSpec: CheckSpec{Request: &Request{
		Auth: &Auth{Type: "bearer", Token: "tok"}, Body: "secret-body", BodySecret: true}}}
	via := []*http.Request{mustReq(t, "POST", "https://example.com/")}
	req := mustReq(t, "POST", "https://evil.test/")
	req.Header.Set("Authorization", "Bearer tok")
	req.Body = io.NopCloser(strings.NewReader("secret-body"))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("secret-body")), nil }
	req.ContentLength = int64(len("secret-body"))
	if err := redirectPolicy(it)(req, via); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatal("auth must be withheld from another host")
	}
	if req.Body != http.NoBody || req.GetBody != nil || req.ContentLength != 0 {
		t.Fatalf("secret body must be dropped, got body=%v getBody=%v len=%d", req.Body, req.GetBody != nil, req.ContentLength)
	}
}

func TestRedirectPolicy_PlainBodyNotStripped(t *testing.T) {
	it := WorkItem{FollowRedirects: true, CheckSpec: CheckSpec{Request: &Request{SecretHeaderNames: []string{"X-Api-Key"}, Body: "plain"}}}
	via := []*http.Request{mustReq(t, "POST", "https://example.com/")}
	req := mustReq(t, "POST", "https://other.test/")
	req.Body = io.NopCloser(strings.NewReader("plain"))
	req.ContentLength = 5
	if err := redirectPolicy(it)(req, via); err != nil {
		t.Fatal(err)
	}
	if req.ContentLength != 5 || req.Body == http.NoBody {
		t.Fatal("a body not marked secret is sent as before")
	}
}

func TestRedirectPolicy_HTTPSToHTTPWithSecretRefused(t *testing.T) {
	for name, it := range map[string]WorkItem{
		"secret header": secretItem(),
		"auth":          {FollowRedirects: true, CheckSpec: CheckSpec{Request: &Request{Auth: &Auth{Type: "basic", Username: "u", Password: "p"}}}},
		"secret body":   {FollowRedirects: true, CheckSpec: CheckSpec{Request: &Request{Body: "b", BodySecret: true}}},
	} {
		via := []*http.Request{mustReq(t, "GET", "https://example.com/")}
		err := redirectPolicy(it)(hop(t, "GET", "http://example.com/"), via)
		if !errors.Is(err, errInsecureRedirect) {
			t.Fatalf("%s: want errInsecureRedirect, got %v", name, err)
		}
	}
}

func TestRedirectPolicy_HTTPSToHTTPWithoutSecretsFollowed(t *testing.T) {
	it := WorkItem{FollowRedirects: true, CheckSpec: CheckSpec{Request: &Request{Headers: map[string]string{"Accept": "x"}}}}
	via := []*http.Request{mustReq(t, "GET", "https://example.com/")}
	if err := redirectPolicy(it)(mustReq(t, "GET", "http://example.com/"), via); err != nil {
		t.Fatalf("no secrets: https→http must still be followed, got %v", err)
	}
	if err := redirectPolicy(WorkItem{FollowRedirects: true})(mustReq(t, "GET", "http://example.com/"), via); err != nil {
		t.Fatalf("nil Request: must be followed, got %v", err)
	}
}

func TestClassifyError_InsecureRedirect(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "http://x", Err: errInsecureRedirect}
	if got := classifyError(err); got != "insecure_redirect" {
		t.Fatalf("got %q", got)
	}
}

// End to end through the real client: a 307 POST from 127.0.0.1 to localhost (a different hostname)
// must arrive with the plain header but without the secret header, auth or secret body.
func TestProbeWith_CrossHostRedirectWithholdsSecrets(t *testing.T) {
	type seen struct{ key, auth, accept, body string }
	got := make(chan seen, 1)
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), r.Header.Get("Accept"), string(b)}
		w.WriteHeader(200)
	}))
	defer dst.Close()
	dstURL := strings.Replace(dst.URL, "127.0.0.1", "localhost", 1)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dstURL, http.StatusTemporaryRedirect)
	}))
	defer src.Close()

	item := WorkItem{MonitorID: "m", Target: src.URL, Method: "POST", ExpectedStatus: "2xx", FollowRedirects: true,
		CheckSpec: CheckSpec{Request: &Request{
			Headers:           map[string]string{"X-Api-Key": "sek", "Accept": "application/json"},
			SecretHeaderNames: []string{"X-Api-Key"},
			Auth:              &Auth{Type: "bearer", Token: "tok"},
			Body:              "secret-body", BodySecret: true,
		}}}
	if res := probeWith(buildClient(item), item); !res.OK {
		t.Fatalf("probe not ok: %+v", res)
	}
	s := <-got
	if s.key != "" || s.auth != "" || s.body != "" {
		t.Fatalf("secrets leaked to the other host: %+v", s)
	}
	if s.accept != "application/json" {
		t.Fatalf("plain header lost: %+v", s)
	}
}

// End to end: an https target redirecting to plain http, on a check with a secret header, is not
// followed — the plain-http server never sees a request and the cause is insecure_redirect.
func TestProbeWith_HTTPSToHTTPRefusedEndToEnd(t *testing.T) {
	hits := make(chan struct{}, 1)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(200)
	}))
	defer plain.Close()
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer tlsSrv.Close()

	item := secretItem()
	item.MonitorID, item.Target, item.ExpectedStatus = "m", tlsSrv.URL, "2xx"
	c := buildClient(item)
	c.Transport = tlsSrv.Client().Transport // trust the test cert; the redirect policy under test is still ours
	res := probeWith(c, item)
	if res.OK || res.Cause == nil || *res.Cause != "insecure_redirect" {
		t.Fatalf("want down with insecure_redirect, got ok=%v cause=%v", res.OK, res.Cause)
	}
	select {
	case <-hits:
		t.Fatal("the plain-http server must never be contacted")
	default:
	}
}

// The diagnose client must apply the same policy.
func TestBuildFreshClient_UsesRedirectPolicy(t *testing.T) {
	c := buildFreshClient(secretItem())
	via := []*http.Request{mustReq(t, "GET", "https://example.com/")}
	if err := c.CheckRedirect(hop(t, "GET", "http://example.com/"), via); !errors.Is(err, errInsecureRedirect) {
		t.Fatalf("fresh client: want errInsecureRedirect, got %v", err)
	}
}
