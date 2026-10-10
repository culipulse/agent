package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestPullSendsCapabilitiesAndVersion(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"agentId":"a1","capabilities":[],"work":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("Pull returned error: %v", err)
	}

	var req PullRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("pull body did not unmarshal to PullRequest: %v (body=%s)", err, gotBody)
	}
	if !reflect.DeepEqual(req.Capabilities, AgentCaps) {
		t.Errorf("pull body capabilities = %v, want %v", req.Capabilities, AgentCaps)
	}
	if req.Version == "" {
		t.Errorf("pull body version is empty; want the build Version")
	}
}

func TestPullSendsArch(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"agentId":"a1","capabilities":[],"work":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("Pull returned error: %v", err)
	}
	var req PullRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("pull body did not unmarshal: %v (body=%s)", err, gotBody)
	}
	if req.Arch == "" {
		t.Errorf("pull body arch is empty; want runtime.GOARCH")
	}
}

func TestPullSendsDiscoveryWhenSet(t *testing.T) {
	var got PullRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"agentId":"a1","capabilities":[],"work":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	c.Discovery = &DiscoveryReport{CIDR: "10.0.4.0/24", Ports: "22,443", AWSRegions: "ap-southeast-1"}
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if got.Discovery == nil {
		t.Fatal("discovery not sent")
	}
	if got.Discovery.CIDR != "10.0.4.0/24" || got.Discovery.Ports != "22,443" || got.Discovery.AWSRegions != "ap-southeast-1" {
		t.Fatalf("discovery mismatch: %+v", got.Discovery)
	}
}

// TestPullAlwaysSendsDiscoveryKey covers the server-side contract from Task 3: the "discovery" key
// must be present on every pull from an updated agent, even when nothing is configured — a present-
// but-empty object means "switched off", while an absent key means "too old to report at all". main()
// always assigns a non-nil (possibly zero-value) *DiscoveryReport to Client.Discovery for exactly this
// reason. This test exercises that mechanic directly: omitempty on the three inner string fields means
// a zero-value DiscoveryReport serialises as "discovery":{}, not as an absent key.
func TestPullAlwaysSendsDiscoveryKey(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_, _ = w.Write([]byte(`{"agentId":"a1","capabilities":[],"work":[]}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	c.Discovery = &DiscoveryReport{} // what main() sends when nothing is configured
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if _, present := raw["discovery"]; !present {
		t.Fatal("discovery key should be present even when the agent has no discovery config, so the server can distinguish 'switched off' from 'too old to report'")
	}
}

// TestPullEnablesSharedGuardOnFirstParty is a regression guard for the load-bearing wiring: the
// server-reported agentKind must flip the dial guard on, and it must happen inside Pull (before any
// work items from this same response are ever probed) — not left to some later, skippable step.
func TestPullEnablesSharedGuardOnFirstParty(t *testing.T) {
	prev := sharedGuard.Load()
	t.Cleanup(func() { sharedGuard.Store(prev) })
	sharedGuard.Store(false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agentId":"a","agentKind":"first_party","capabilities":[],"work":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("Pull returned error: %v", err)
	}
	if !sharedGuard.Load() {
		t.Fatal("want sharedGuard on after a pull response reporting agentKind=first_party")
	}
}

// TestPullLeavesSharedGuardOffForTenant proves a tenant response does not turn the guard on —
// tenant-owned agents must keep reaching private targets, which is their job.
func TestPullLeavesSharedGuardOffForTenant(t *testing.T) {
	prev := sharedGuard.Load()
	t.Cleanup(func() { sharedGuard.Store(prev) })
	sharedGuard.Store(false)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agentId":"a","agentKind":"tenant","capabilities":[],"work":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("Pull returned error: %v", err)
	}
	if sharedGuard.Load() {
		t.Fatal("want sharedGuard to stay off after a pull response reporting agentKind=tenant")
	}
}

// TestPullKeepsSharedGuardStickyOnTenantResponse proves the guard is sticky: once on, a later pull
// that reports "tenant" (e.g. a misconfigured server, or a race during a kind change) must never turn
// it back off.
func TestPullKeepsSharedGuardStickyOnTenantResponse(t *testing.T) {
	prev := sharedGuard.Load()
	t.Cleanup(func() { sharedGuard.Store(prev) })
	sharedGuard.Store(true)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"agentId":"a","agentKind":"tenant","capabilities":[],"work":[]}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.Pull(context.Background()); err != nil {
		t.Fatalf("Pull returned error: %v", err)
	}
	if !sharedGuard.Load() {
		t.Fatal("want sharedGuard to stay on (sticky) even though this pull reported agentKind=tenant")
	}
}

func TestDoCapsResponseBody(t *testing.T) {
	// Valid JSON far larger than a shrunk cap: {"x":"aaaa...a"}. Capped → truncated mid-string →
	// invalid JSON → do() errors; large cap → full parse → success. Deleting the io.LimitReader
	// wrap in client.go makes case (1) parse successfully → this test fails. That's the discriminator.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"x":"`))
		_, _ = w.Write(bytes.Repeat([]byte("a"), 4<<20))
		_, _ = w.Write([]byte(`"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	old := maxRespBytes
	defer func() { maxRespBytes = old }()

	// (1) small cap → body truncated → invalid JSON → error.
	maxRespBytes = 1024
	var out map[string]any
	if err := c.do(context.Background(), "/x", nil, &out); err == nil {
		t.Fatal("expected error: capped read should truncate the body into invalid JSON")
	}

	// (2) large cap → full valid JSON parses → no error, value round-trips.
	maxRespBytes = 8 << 20
	out = nil
	if err := c.do(context.Background(), "/x", nil, &out); err != nil {
		t.Fatalf("expected success with a large cap, got %v", err)
	}
	if s, _ := out["x"].(string); len(s) != 4<<20 {
		t.Fatalf("expected 4 MiB string to round-trip uncapped, got len %d", len(s))
	}
}

func TestCloudResourcesSendsScannedPairsOnlyWhenGiven(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"upserted":0,"closed":0}`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")

	// An agent that reports per-pair results sends the list, even an empty one.
	if err := c.CloudResources(context.Background(), "aws", nil, "ec2:DescribeInstances (r1): AccessDenied", []ScannedPair{}); err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(gotBody, &body)
	if string(body["scanned"]) != "[]" {
		t.Fatalf("an empty scanned list must still be sent, got %s", gotBody)
	}

	if err := c.CloudResources(context.Background(), "aws", nil, "", []ScannedPair{{ResourceType: "ec2", Region: "r1"}}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(gotBody, []byte(`"scanned":[{"resourceType":"ec2","region":"r1"}]`)) {
		t.Fatalf("scanned pair not sent: %s", gotBody)
	}

	// nil (no per-pair data) leaves the field out entirely.
	if err := c.CloudResources(context.Background(), "aws", nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(gotBody, []byte(`"scanned"`)) {
		t.Fatalf("nil scanned must be omitted: %s", gotBody)
	}
}
