package main

import "testing"

func TestRequiredCap(t *testing.T) {
	cases := []struct {
		item WorkItem
		want string
	}{
		{WorkItem{Type: "http"}, "http"},
		{WorkItem{Type: ""}, "http"},
		{WorkItem{Type: "tcp"}, "tcp"},
		{WorkItem{Type: "icmp"}, "icmp"},
		{WorkItem{Type: "udp"}, "udp"},
		{WorkItem{Type: "http", CheckSpec: CheckSpec{Assertions: []Assertion{{Source: "cert_expiry"}}}}, "cert"},
		// Intentional single-primary-cap behavior: the type switch wins over assertions,
		// so a tcp monitor carrying a cert_expiry assertion resolves to "tcp", not "cert".
		// (The server never routes such a combo to an agent; documented in requiredCap.)
		{WorkItem{Type: "tcp", CheckSpec: CheckSpec{Assertions: []Assertion{{Source: "cert_expiry"}}}}, "tcp"},
	}
	for _, c := range cases {
		if got := requiredCap(c.item); got != c.want {
			t.Errorf("requiredCap(%+v) = %q, want %q", c.item, got, c.want)
		}
	}
}

func TestSupports(t *testing.T) {
	caps := []string{"http", "cert"}
	if !supports(caps, WorkItem{Type: "http"}) {
		t.Error("http should be supported by [http cert]")
	}
	if !supports(caps, WorkItem{Type: "http", CheckSpec: CheckSpec{Assertions: []Assertion{{Source: "cert_expiry"}}}}) {
		t.Error("cert_expiry should be supported by [http cert]")
	}
	if supports(caps, WorkItem{Type: "tcp"}) {
		t.Error("tcp should NOT be supported by [http cert]")
	}
	if !supports([]string{"tcp"}, WorkItem{Type: "tcp"}) {
		t.Error("tcp should be supported by [tcp]")
	}
}

func TestPlatformTypesAbstain(t *testing.T) {
	for _, typ := range []string{"domain", "dns", "vendor"} {
		item := WorkItem{MonitorID: "m1", Type: typ}
		if supports(AgentCaps, item) {
			t.Fatalf("agent must not claim support for platform-probed type %q", typ)
		}
	}
}

func TestAgentCapsAreKnownAndComplete(t *testing.T) {
	known := map[string]bool{"http": true, "cert": true, "tcp": true, "icmp": true, "udp": true}
	for _, c := range AgentCaps {
		if !known[c] {
			t.Errorf("AgentCaps contains unknown capability %q", c)
		}
	}
	if len(AgentCaps) != len(known) {
		t.Errorf("AgentCaps = %v; expected all %d implemented prober caps", AgentCaps, len(known))
	}
}
