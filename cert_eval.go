package main

import (
	"strconv"
	"strings"
)

func statusMatches(status int, expected string) bool {
	if expected == "" {
		expected = "2xx"
	}
	for _, part := range strings.Split(expected, ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		// Match any Nxx wildcard (e.g. "2xx", "4xx", "5xx") where N is a single digit 0-9.
		if len(part) == 3 && part[1] == 'x' && part[2] == 'x' && part[0] >= '0' && part[0] <= '9' {
			d := int(part[0] - '0')
			if status >= d*100 && status < d*100+100 {
				return true
			}
			continue
		}
		if n, err := strconv.Atoi(part); err == nil && n == status {
			return true
		}
	}
	return false
}

func classify(statusOK bool, status int, latencyMs int64, daysRemaining *float64, assertions []Assertion, body string, headers map[string]string) (bool, bool, string) {
	if !statusOK {
		return false, false, "status_" + strconv.Itoa(status)
	}
	// Content assertions (text_body/json_body/header) are hard failures => DOWN, mirroring the edge
	// (src/lib/classify.ts). They short-circuit before the degraded (response_time/cert) checks.
	if cause, ok := evalContent(assertions, body, headers); !ok {
		return false, false, cause
	}
	degraded := false
	cause := ""
	for _, a := range assertions {
		v, err := strconv.ParseFloat(a.Value, 64)
		if err != nil {
			continue
		}
		switch a.Source {
		case "response_time":
			if (a.Op == "lt" && float64(latencyMs) >= v) || (a.Op == "gt" && float64(latencyMs) <= v) {
				degraded = true
				if cause == "" {
					cause = "slow"
				}
			}
		case "cert_expiry":
			if daysRemaining == nil {
				continue
			}
			if (a.Op == "gt" && *daysRemaining <= v) || (a.Op == "lt" && *daysRemaining >= v) {
				degraded = true
				if cause == "" {
					cause = "cert_expiring"
				}
			}
		}
	}
	return true, degraded, cause
}
