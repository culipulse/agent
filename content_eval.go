package main

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// content_eval.go mirrors src/lib/assert.ts (and src/lib/jsonpath.ts). Keep the two in lockstep —
// the edge and an agent must agree on the same content assertion for the same response. NOTE: Go's
// regexp is RE2; JS RegExp is not — `matches` patterns should stay RE2-compatible (no backrefs /
// lookaround). Most content checks use `contains`.

var jsonPathKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]$`)

func safeRegexMatch(pattern, input string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(input)
}

// jsNumberRe matches the decimal forms JS `Number()` parses to a FINITE value — what assert.ts's
// `Number.isFinite(Number(x))` accepts in practice. Intentionally stricter than Go's ParseFloat: it
// rejects Inf/NaN and hex/octal/binary/underscore literals, so the agent never treats a value as
// numeric when the edge wouldn't (those forms can't appear in valid JSON and are pathological as
// user-supplied comparison values). The empty string is rejected here, mirroring the `trim() !== ""`
// guard the edge applies before its isFinite check.
var jsNumberRe = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

func isJSNumeric(s string) bool { return jsNumberRe.MatchString(strings.TrimSpace(s)) }

func bothNumeric(a, b string) bool { return isJSNumeric(a) && isJSNumeric(b) }

func mustF(s string) float64 { f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64); return f }

// valuePasses mirrors assert.ts valuePasses (json_body + header). `found` reports whether the lookup
// existed (TS `undefined`), so `exists` and the "undefined => false" rule are reproduced exactly.
func valuePasses(op, v string, found bool, value string) bool {
	if op == "exists" {
		return found
	}
	if !found {
		return false
	}
	switch op {
	case "equals":
		if bothNumeric(v, value) {
			return mustF(v) == mustF(value)
		}
		return v == value
	case "not_equals":
		if bothNumeric(v, value) {
			return mustF(v) != mustF(value)
		}
		return v != value
	case "contains":
		return strings.Contains(v, value)
	case "gt":
		return bothNumeric(v, value) && mustF(v) > mustF(value)
	case "lt":
		return bothNumeric(v, value) && mustF(v) < mustF(value)
	case "matches":
		return safeRegexMatch(value, v)
	default:
		return false
	}
}

// parsePath mirrors src/lib/jsonpath.ts parsePath: "$" => [], dot keys [A-Za-z0-9_-], and [n] /
// ['k'] / ["k"] indices. Anything else (wildcards/slices/filters) => not-ok.
func parsePath(path string) ([]any, bool) {
	if path == "$" {
		return []any{}, true
	}
	if path == "" || path[0] != '$' {
		return nil, false
	}
	segs := []any{}
	i := 1
	for i < len(path) {
		switch path[i] {
		case '.':
			i++
			key := ""
			for i < len(path) && jsonPathKeyRe.MatchString(string(path[i])) {
				key += string(path[i])
				i++
			}
			if key == "" {
				return nil, false
			}
			segs = append(segs, key)
		case '[':
			rel := strings.IndexByte(path[i:], ']')
			if rel == -1 {
				return nil, false
			}
			end := i + rel
			inner := path[i+1 : end]
			i = end + 1
			if isDigits(inner) {
				n, _ := strconv.Atoi(inner)
				segs = append(segs, n)
			} else if len(inner) >= 2 && ((inner[0] == '\'' && inner[len(inner)-1] == '\'') || (inner[0] == '"' && inner[len(inner)-1] == '"')) {
				segs = append(segs, inner[1:len(inner)-1])
			} else {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return segs, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// extractJSON returns the value at path as a string + whether it was found, mirroring extract() plus
// the TS `String(v)` / `JSON.stringify(v)` coercion that precedes valuePasses.
func extractJSON(root any, path string) (string, bool) {
	segs, ok := parsePath(path)
	if !ok {
		return "", false
	}
	cur := root
	for _, s := range segs {
		// Traversing INTO a null/absent value yields not-found, mirroring TS extract()'s
		// `if (cur == null) return undefined`. (A null *leaf* at the end is still found — below.)
		if cur == nil {
			return "", false
		}
		switch seg := s.(type) {
		case int:
			arr, ok := cur.([]any)
			if !ok || seg < 0 || seg >= len(arr) {
				return "", false
			}
			cur = arr[seg]
		case string:
			obj, ok := cur.(map[string]any)
			if !ok {
				return "", false
			}
			val, present := obj[seg]
			if !present {
				return "", false
			}
			cur = val
		}
	}
	// cur may be nil here — a JSON null *leaf*. That is FOUND with value "null", matching the edge:
	// extract() returns the raw null and JSON.stringify(null) === "null". (Do NOT treat it as absent.)
	return stringify(cur), true
}

// stringify mirrors `typeof v === "string" ? v : JSON.stringify(v)`. JSON numbers decode to float64;
// render integers without a trailing ".0" so "5" compares equal to value "5".
func stringify(v any) string {
	switch t := v.(type) {
	case nil:
		return "null" // JSON null leaf — matches JSON.stringify(null) === "null"
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// evalContent evaluates every content assertion (text_body/json_body/header). ALL must pass; returns
// the first failure's cause. Non-content sources are ignored here (classify handles response_time /
// cert_expiry). With no content assertions it returns ("", true).
func evalContent(assertions []Assertion, body string, headers map[string]string) (string, bool) {
	var jsonParsed, jsonOK bool
	var jsonRoot any
	// parseJSON reports the decoded body + whether it parsed. The OK flag mirrors the edge's
	// `json() === undefined` check: invalid JSON => no lookup (not-found), but a valid `null` body
	// still parses (jsonOK=true, jsonRoot=nil) so `$` exists / null-leaf semantics match.
	parseJSON := func() (any, bool) {
		if !jsonParsed {
			jsonParsed = true
			jsonOK = json.Unmarshal([]byte(body), &jsonRoot) == nil
		}
		return jsonRoot, jsonOK
	}
	for _, a := range assertions {
		switch a.Source {
		case "text_body":
			switch a.Op {
			case "contains":
				if !strings.Contains(body, a.Value) {
					return "assert_text_body_contains", false
				}
			case "not_contains":
				if strings.Contains(body, a.Value) {
					return "assert_text_body_not_contains", false
				}
			case "matches":
				if !safeRegexMatch(a.Value, body) {
					return "assert_text_body_matches", false
				}
			}
		case "json_body":
			var v string
			var found bool
			if root, ok := parseJSON(); ok {
				v, found = extractJSON(root, a.Path)
			}
			if !valuePasses(a.Op, v, found, a.Value) {
				return "assert_json_body_" + a.Op, false
			}
		case "header":
			v, found := headers[strings.ToLower(a.Name)]
			if !valuePasses(a.Op, v, found, a.Value) {
				return "assert_header_" + a.Op, false
			}
		}
	}
	return "", true
}
