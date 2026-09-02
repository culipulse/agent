package main

import "time"

// AgentCaps is the set of capabilities this binary implements (its probers): the agent advertises
// these on /agent/v1/pull, and the worker reconciles them into the stored agent caps. Keep in sync
// with AGENT_CAPS / CAPABILITY_MATRIX in src/lib (same sync note as requiredCap).
var AgentCaps = []string{"http", "cert", "tcp", "icmp", "udp"}

// abstainCause is the single cause the worker treats as a non-vote (drops on ingest) rather than a
// "down" — mirrors ABSTAIN_CAUSE in src/lib/agent-abstain.ts. Used for both capability abstains (G5)
// and panic-recovery abstains: a result the agent couldn't produce is equivalent to a missing one.
const abstainCause = "unsupported_by_agent"

// abstainResult builds the non-vote an agent returns when it has no usable verdict for a work item.
func abstainResult(item WorkItem) IngestResult {
	c := abstainCause
	return IngestResult{MonitorID: item.MonitorID, TS: time.Now().Unix(), OK: false, Cause: &c}
}

// unsupportedResult is the abstain an agent returns for a work item whose capability it does not
// advertise (G5). The worker treats cause "unsupported_by_agent" as a non-vote, never a "down".
func unsupportedResult(item WorkItem) IngestResult {
	return abstainResult(item)
}

// requiredCap returns the single primary capability a work item needs, mirroring
// the server capability matrix in src/lib/capabilities.ts. tcp/icmp/udp are typed
// checks; a cert_expiry assertion needs "cert"; everything else is plain http.
// Keep this in sync with CAPABILITY_MATRIX (the snapshot test guards the TS side).
//
// Unlike the TS requiredCaps (which returns the full SET of caps — e.g. a tcp
// monitor carrying a cert_expiry assertion needs both "tcp" AND "cert"), this
// returns only the type's primary cap: the type switch wins over assertions. That
// is sufficient because the server pre-routes work, never assigning an agent a
// type/assertion combo it can't fully service; the agent only re-checks the
// primary cap as defense-in-depth.
//
// Known types in CAPABILITY_MATRIX.byType: http, tcp, icmp, udp, domain, dns, vendor.
// Add a case here whenever a new type is added to the matrix, or the mirror will
// silently fall through to "http".
func requiredCap(item WorkItem) string {
	switch item.Type {
	case "tcp":
		return "tcp"
	case "icmp":
		return "icmp"
	case "udp":
		return "udp"
	// Platform-probed types (domain/dns/vendor) are never routed to an agent. Naming them
	// explicitly means a mis-routed item abstains via supports()==false instead of falling
	// through to the HTTP prober and reporting a meaningless verdict. Defence in depth.
	case "domain":
		return "domain"
	case "dns":
		return "dns"
	case "vendor":
		return "vendor"
	}
	for _, a := range item.CheckSpec.Assertions {
		if a.Source == "cert_expiry" {
			return "cert"
		}
	}
	return "http"
}

// supports reports whether an agent advertising caps can service this work item.
func supports(caps []string, item WorkItem) bool {
	need := requiredCap(item)
	for _, c := range caps {
		if c == need {
			return true
		}
	}
	return false
}
