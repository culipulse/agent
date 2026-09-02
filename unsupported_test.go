package main

import "testing"

func TestUnsupportedResult(t *testing.T) {
	r := unsupportedResult(WorkItem{MonitorID: "m1", Type: "icmp"})
	if r.MonitorID != "m1" {
		t.Fatalf("MonitorID = %q, want m1", r.MonitorID)
	}
	if r.OK {
		t.Fatalf("OK = true, want false")
	}
	if r.Cause == nil || *r.Cause != "unsupported_by_agent" {
		t.Fatalf("Cause = %v, want unsupported_by_agent", r.Cause)
	}
	if r.TS == 0 {
		t.Fatalf("TS not set")
	}
}
