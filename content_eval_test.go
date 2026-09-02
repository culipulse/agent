package main

import "testing"

func TestEvalContentTextBody(t *testing.T) {
	h := map[string]string{}
	cases := []struct {
		name   string
		a      Assertion
		body   string
		wantOK bool
	}{
		{"contains hit", Assertion{Source: "text_body", Op: "contains", Value: "ok"}, "all ok here", true},
		{"contains miss", Assertion{Source: "text_body", Op: "contains", Value: "ok"}, "nope", false},
		{"not_contains hit", Assertion{Source: "text_body", Op: "not_contains", Value: "err"}, "fine", true},
		{"not_contains miss", Assertion{Source: "text_body", Op: "not_contains", Value: "err"}, "err!", false},
		{"matches hit", Assertion{Source: "text_body", Op: "matches", Value: "^a.*z$"}, "abcz", true},
		{"matches miss", Assertion{Source: "text_body", Op: "matches", Value: "^a.*z$"}, "abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := evalContent([]Assertion{tc.a}, tc.body, h)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v", ok, tc.wantOK)
			}
		})
	}
}

func TestEvalContentJSONBody(t *testing.T) {
	body := `{"status":"green","count":5,"nested":{"k":"v"},"arr":[10,20]}`
	h := map[string]string{}
	cases := []struct {
		a      Assertion
		wantOK bool
	}{
		{Assertion{Source: "json_body", Op: "equals", Path: "$.status", Value: "green"}, true},
		{Assertion{Source: "json_body", Op: "equals", Path: "$.status", Value: "red"}, false},
		{Assertion{Source: "json_body", Op: "gt", Path: "$.count", Value: "3"}, true},
		{Assertion{Source: "json_body", Op: "lt", Path: "$.count", Value: "3"}, false},
		{Assertion{Source: "json_body", Op: "equals", Path: "$.nested.k", Value: "v"}, true},
		{Assertion{Source: "json_body", Op: "equals", Path: "$.arr[1]", Value: "20"}, true},
		{Assertion{Source: "json_body", Op: "exists", Path: "$.count", Value: ""}, true},
		{Assertion{Source: "json_body", Op: "exists", Path: "$.missing", Value: ""}, false},
		{Assertion{Source: "json_body", Op: "contains", Path: "$.status", Value: "ree"}, true},
	}
	for _, tc := range cases {
		_, ok := evalContent([]Assertion{tc.a}, body, h)
		if ok != tc.wantOK {
			t.Fatalf("%s %s %q: ok=%v want %v", tc.a.Op, tc.a.Path, tc.a.Value, ok, tc.wantOK)
		}
	}
}

func TestEvalContentHeader(t *testing.T) {
	h := map[string]string{"content-type": "application/json", "x-env": "prod"}
	cases := []struct {
		a      Assertion
		wantOK bool
	}{
		{Assertion{Source: "header", Op: "equals", Name: "X-Env", Value: "prod"}, true},
		{Assertion{Source: "header", Op: "contains", Name: "Content-Type", Value: "json"}, true},
		{Assertion{Source: "header", Op: "exists", Name: "X-Env", Value: ""}, true},
		{Assertion{Source: "header", Op: "exists", Name: "X-Missing", Value: ""}, false},
	}
	for _, tc := range cases {
		_, ok := evalContent([]Assertion{tc.a}, "", h)
		if ok != tc.wantOK {
			t.Fatalf("%s %s: ok=%v want %v", tc.a.Op, tc.a.Name, ok, tc.wantOK)
		}
	}
}

// Invalid JSON body: a json_body assertion can't be satisfied (value not found), but `exists`
// against a missing value is false — mirrors the edge (json() returns undefined).
func TestEvalContentInvalidJSON(t *testing.T) {
	_, ok := evalContent([]Assertion{{Source: "json_body", Op: "equals", Path: "$.x", Value: "1"}}, "not json", nil)
	if ok {
		t.Fatalf("expected fail on unparseable json body")
	}
}

// No content assertions => pass (empty/none is not a failure).
func TestEvalContentNone(t *testing.T) {
	if _, ok := evalContent([]Assertion{{Source: "response_time", Op: "lt", Value: "5"}}, "x", nil); !ok {
		t.Fatalf("non-content assertions must not fail content eval")
	}
}

// A JSON null *leaf* must be FOUND with value "null" (edge: extract() returns null; JSON.stringify(null)
// === "null"). A missing key is not-found. This is the consensus-divergence the reviewer flagged.
func TestEvalContentJSONNullLeaf(t *testing.T) {
	body := `{"x":null,"y":1}`
	cases := []struct {
		a      Assertion
		wantOK bool
	}{
		{Assertion{Source: "json_body", Op: "exists", Path: "$.x"}, true}, // present, value null
		{Assertion{Source: "json_body", Op: "equals", Path: "$.x", Value: "null"}, true},
		{Assertion{Source: "json_body", Op: "contains", Path: "$.x", Value: "ul"}, true},
		{Assertion{Source: "json_body", Op: "exists", Path: "$.missing"}, false}, // absent key
		{Assertion{Source: "json_body", Op: "equals", Path: "$.missing", Value: "null"}, false},
	}
	for _, tc := range cases {
		if _, ok := evalContent([]Assertion{tc.a}, body, nil); ok != tc.wantOK {
			t.Fatalf("%s %s: ok=%v want %v", tc.a.Op, tc.a.Path, ok, tc.wantOK)
		}
	}
}

// Invalid JSON => no lookup (not-found), but a valid `null` body parses so `$ exists` is true.
func TestEvalContentJSONRootAndInvalid(t *testing.T) {
	if _, ok := evalContent([]Assertion{{Source: "json_body", Op: "exists", Path: "$"}}, "null", nil); !ok {
		t.Fatalf("valid null body: $ should exist")
	}
	if _, ok := evalContent([]Assertion{{Source: "json_body", Op: "exists", Path: "$.x"}}, "null", nil); ok {
		t.Fatalf("null body: $.x should not exist")
	}
	if _, ok := evalContent([]Assertion{{Source: "json_body", Op: "exists", Path: "$.x"}}, "<<not json>>", nil); ok {
		t.Fatalf("invalid json: $.x should not exist")
	}
}

// bothNumeric must match JS Number.isFinite(Number(x)): Inf/NaN/hex are NOT numeric, so a gt/lt
// against such a value falls back to a (failing) numeric compare, never a spurious pass.
func TestEvalContentNumericCoercion(t *testing.T) {
	body := `{"count":5}`
	// "5" gt "Infinity" — not both-numeric => gt false => assertion fails
	if _, ok := evalContent([]Assertion{{Source: "json_body", Op: "gt", Path: "$.count", Value: "Infinity"}}, body, nil); ok {
		t.Fatalf("gt vs Infinity must not pass")
	}
	// "5" equals "5.0" — both numeric => equal
	if _, ok := evalContent([]Assertion{{Source: "json_body", Op: "equals", Path: "$.count", Value: "5.0"}}, body, nil); !ok {
		t.Fatalf("5 equals 5.0 (numeric) must pass")
	}
	// "5" equals "0x5" — 0x5 is not JS-numeric => string compare "5" != "0x5" => fail
	if _, ok := evalContent([]Assertion{{Source: "json_body", Op: "equals", Path: "$.count", Value: "0x5"}}, body, nil); ok {
		t.Fatalf("5 equals 0x5 must not pass (hex is not JS-numeric)")
	}
}

func TestEvalContentNotEquals(t *testing.T) {
	if _, ok := evalContent([]Assertion{{Source: "json_body", Op: "not_equals", Path: "$.s", Value: "a"}}, `{"s":"b"}`, nil); !ok {
		t.Fatalf("json not_equals (b != a) must pass")
	}
	if _, ok := evalContent([]Assertion{{Source: "header", Op: "not_equals", Name: "X-Env", Value: "prod"}}, "", map[string]string{"x-env": "stg"}); !ok {
		t.Fatalf("header not_equals (stg != prod) must pass")
	}
}
