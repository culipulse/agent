package main

import "testing"

func TestStatusMatches(t *testing.T) {
	cases := []struct {
		s    int
		exp  string
		want bool
	}{
		{200, "2xx", true}, {204, "2xx", true}, {301, "2xx", false},
		{200, "200", true}, {301, "200,301", true}, {404, "2xx", false},
		{200, "", true}, {302, "3xx", true},
		// BUG C2: any Nxx wildcard must match like the worker (0-9 leading digit)
		{404, "4xx", true},
		{400, "4xx", true},
		{499, "4xx", true},
		{503, "5xx", true},
		{500, "5xx", true},
		{200, "4xx", false},
		{404, "5xx", false},
	}
	for _, c := range cases {
		if got := statusMatches(c.s, c.exp); got != c.want {
			t.Errorf("statusMatches(%d,%q)=%v want %v", c.s, c.exp, got, c.want)
		}
	}
}

func TestClassify(t *testing.T) {
	d := func(x float64) *float64 { return &x }
	if ok, deg, cause := classify(false, 500, 10, nil, nil, "", nil); ok || deg || cause != "status_500" {
		t.Errorf("down: %v %v %q", ok, deg, cause)
	}
	if ok, deg, cause := classify(true, 200, 10, d(90), nil, "", nil); !ok || deg || cause != "" {
		t.Errorf("up: %v %v %q", ok, deg, cause)
	}
	if ok, deg, _ := classify(true, 200, 10, d(90), []Assertion{{Source: "cert_expiry", Op: "gt", Value: "14"}}, "", nil); !ok || deg {
		t.Errorf("cert ok: %v %v", ok, deg)
	}
	if ok, deg, cause := classify(true, 200, 10, d(90), []Assertion{{Source: "cert_expiry", Op: "gt", Value: "100"}}, "", nil); !ok || !deg || cause != "cert_expiring" {
		t.Errorf("cert degraded: %v %v %q", ok, deg, cause)
	}
	if ok, deg, cause := classify(true, 200, 10, nil, []Assertion{{Source: "response_time", Op: "lt", Value: "5"}}, "", nil); !ok || !deg || cause != "slow" {
		t.Errorf("slow: %v %v %q", ok, deg, cause)
	}
}

func TestClassifyContentDown(t *testing.T) {
	// A content assertion failure is DOWN (ok=false), not degraded.
	ok, deg, cause := classify(true, 200, 10, nil,
		[]Assertion{{Source: "text_body", Op: "contains", Value: "ready"}}, "not here", nil)
	if ok || deg || cause != "assert_text_body_contains" {
		t.Fatalf("content down: ok=%v deg=%v cause=%q", ok, deg, cause)
	}
	// Content passes + a breached response_time => degraded (content didn't fail).
	ok, deg, cause = classify(true, 200, 999, nil,
		[]Assertion{{Source: "text_body", Op: "contains", Value: "ok"}, {Source: "response_time", Op: "lt", Value: "5"}},
		"ok!", nil)
	if !ok || !deg || cause != "slow" {
		t.Fatalf("content ok + slow: ok=%v deg=%v cause=%q", ok, deg, cause)
	}
	// header assertion satisfied from the captured headers.
	ok, _, _ = classify(true, 200, 10, nil,
		[]Assertion{{Source: "header", Op: "equals", Name: "X-Env", Value: "prod"}}, "", map[string]string{"x-env": "prod"})
	if !ok {
		t.Fatalf("header pass: ok=%v", ok)
	}
}
