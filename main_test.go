package main

import "testing"

// TestBuildDiscoveryReportUsesRawPorts guards the load-bearing detail that report.Ports must carry the
// operator's raw CULIPULSE_DISCOVER_PORTS string, not parsePorts' parsed/sorted/deduped output — so the
// console shows what the operator actually typed. The env value below has whitespace and a duplicate,
// which parsePorts would visibly transform (trim, dedupe, sort to "22,443"): if buildDiscoveryReport
// ever wired in the parsed form instead, this test would fail.
func TestBuildDiscoveryReportUsesRawPorts(t *testing.T) {
	t.Setenv("CULIPULSE_DISCOVER_CIDR", "10.0.4.0/24")
	t.Setenv("CULIPULSE_DISCOVER_PORTS", " 443, 22, 22")
	t.Setenv("CULIPULSE_AWS_SD", "")

	report := buildDiscoveryReport()

	if report.CIDR != "10.0.4.0/24" {
		t.Fatalf("CIDR = %q, want the configured CIDR", report.CIDR)
	}
	if report.Ports != " 443, 22, 22" {
		t.Fatalf("Ports = %q, want the raw env string unchanged (parsePorts would have produced \"22,443\")", report.Ports)
	}
}

// TestBuildDiscoveryReportUsesResolvedRegions guards the other load-bearing detail: report.AWSRegions
// must carry parseRegions' resolved list, which folds in the AWS_REGION fallback when
// CULIPULSE_AWS_REGIONS is unset — so the console shows what is really being scanned. Only setting
// AWS_REGION (leaving CULIPULSE_AWS_REGIONS empty) is exactly the case that proves the fallback is
// actually used rather than the raw (here, empty) CULIPULSE_AWS_REGIONS value.
func TestBuildDiscoveryReportUsesResolvedRegions(t *testing.T) {
	t.Setenv("CULIPULSE_DISCOVER_CIDR", "")
	t.Setenv("CULIPULSE_AWS_SD", "1")
	t.Setenv("CULIPULSE_AWS_REGIONS", "")
	t.Setenv("AWS_REGION", "ap-southeast-1")

	report := buildDiscoveryReport()

	if report.AWSRegions != "ap-southeast-1" {
		t.Fatalf("AWSRegions = %q, want the AWS_REGION fallback resolved by parseRegions", report.AWSRegions)
	}
}

// TestBuildDiscoveryReportZeroValueWhenNothingConfigured covers R1: an agent with nothing configured
// must still produce a non-nil, zero-valued report (not nil) — main() always sends it, so the server
// sees "discovery":{} rather than a missing key, distinguishing "switched off" from "too old to report".
func TestBuildDiscoveryReportZeroValueWhenNothingConfigured(t *testing.T) {
	t.Setenv("CULIPULSE_DISCOVER_CIDR", "")
	t.Setenv("CULIPULSE_DISCOVER_PORTS", "")
	t.Setenv("CULIPULSE_AWS_SD", "")
	t.Setenv("CULIPULSE_AWS_REGIONS", "")
	t.Setenv("AWS_REGION", "")

	report := buildDiscoveryReport()

	if report == nil {
		t.Fatal("buildDiscoveryReport returned nil; want a non-nil zero-valued report")
	}
	if report.CIDR != "" || report.Ports != "" || report.AWSRegions != "" {
		t.Fatalf("report = %+v, want all-empty fields when nothing is configured", report)
	}
}

// TestSharedGuardFromEnv covers the CULIPULSE_SHARED_AGENT defence-in-depth switch: a shared-agent
// host sets it directly in its env file so the guard is on even before the first successful /pull.
func TestSharedGuardFromEnv(t *testing.T) {
	t.Setenv("CULIPULSE_SHARED_AGENT", "1")
	if !sharedGuardFromEnv() {
		t.Fatal("want true when CULIPULSE_SHARED_AGENT=1")
	}
}

func TestSharedGuardFromEnvExplicitZero(t *testing.T) {
	t.Setenv("CULIPULSE_SHARED_AGENT", "0")
	if sharedGuardFromEnv() {
		t.Fatal("want false when CULIPULSE_SHARED_AGENT=0")
	}
}

func TestSharedGuardFromEnvUnset(t *testing.T) {
	t.Setenv("CULIPULSE_SHARED_AGENT", "")
	if sharedGuardFromEnv() {
		t.Fatal("want false when CULIPULSE_SHARED_AGENT is unset")
	}
}
