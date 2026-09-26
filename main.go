package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
)

// Version is the agent build version, stamped at build time via -ldflags "-X main.Version=<v>".
// Defaults to "dev" for un-stamped local builds. Reported to the server on /agent/v1/pull.
var Version = "dev"

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log.Printf("culipulse-agent %s starting", Version)
	if initSentry() {
		defer sentry.Flush(2 * time.Second)
		log.Printf("culipulse-agent: sentry telemetry enabled")
	}
	base := os.Getenv("CULIPULSE_API_URL")
	token := os.Getenv("CULIPULSE_AGENT_TOKEN")
	if base == "" || token == "" {
		log.Fatal("CULIPULSE_API_URL and CULIPULSE_AGENT_TOKEN are required")
	}
	// Defence-in-depth: a shared-agent host sets CULIPULSE_SHARED_AGENT=1 so the dial guard is on
	// from process start, ahead of (and independent of) whatever the server reports on the first
	// /pull (applyAgentKind, in dialguard.go, covers that server-driven path).
	if sharedGuardFromEnv() {
		enableSharedGuard("env")
	}
	poll, _ := strconv.Atoi(getenv("CULIPULSE_POLL_SECONDS", "30"))
	if poll <= 0 {
		poll = 30
	}
	// Default worker-pool size. Measured (2026-06-25): the agent is I/O-bound, so a higher pool costs
	// little for TCP/light checks (~tens of MB RSS) while ~doubling throughput. 100 is a safe out-of-box
	// 2× that won't overwhelm a small host even on HTTP+TLS loads; raise CULIPULSE_MAX_CONCURRENCY on
	// beefy agents (200-500) — pair it with a higher per-agent max_checks_per_min on the server.
	maxc, _ := strconv.Atoi(getenv("CULIPULSE_MAX_CONCURRENCY", "100"))
	if maxc <= 0 {
		maxc = 100
	}
	client := NewClient(base, token)
	log.Printf("culipulse-agent: api=%s poll=%ds maxc=%d", base, poll, maxc)

	// Cancel on SIGINT/SIGTERM so the engine can drain in-flight probes and flush pending results.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// What this agent is configured to scan, reported on every pull — even zero-valued — so the
	// "discovery" key is always present on the pull body; the server reads a present-but-empty object
	// as "switched off" and an absent key as "too old to report", so an updated agent must never omit
	// the key just because scanning is off. Built by buildDiscoveryReport (kept separate from the
	// goroutine-launch blocks below, and independently testable via t.Setenv) from the same env vars
	// those blocks read to decide whether to start background discovery loops. Assigned before
	// runEngine (below) so the client.Pull method value it passes sees it.
	client.Discovery = buildDiscoveryReport()

	if cidr := os.Getenv("CULIPULSE_DISCOVER_CIDR"); cidr != "" {
		ports := parsePorts(os.Getenv("CULIPULSE_DISCOVER_PORTS"))
		ds, _ := strconv.Atoi(getenv("CULIPULSE_DISCOVER_SECONDS", "300"))
		if ds <= 0 {
			ds = 300
		}
		log.Printf("discovery enabled: cidr=%s ports=%d every=%ds", cidr, len(ports), ds)
		go everyUntil(ctx, time.Duration(ds)*time.Second, func() { runDiscovery(client, cidr, ports, 100) })
	}
	if os.Getenv("CULIPULSE_AWS_SD") != "" {
		regions := parseRegions(os.Getenv("CULIPULSE_AWS_REGIONS"), os.Getenv("AWS_REGION"))
		as, _ := strconv.Atoi(getenv("CULIPULSE_AWS_SD_SECONDS", "300"))
		if as <= 0 {
			as = 300
		}
		log.Printf("aws discovery enabled: regions=%v every=%ds", regions, as)
		go everyUntil(ctx, time.Duration(as)*time.Second, func() { runAWSDiscovery(client, regions) })
	}

	// Periodic activity summary cadence; 0 disables it (CULIPULSE_SUMMARY_SECONDS).
	summary, _ := strconv.Atoi(getenv("CULIPULSE_SUMMARY_SECONDS", "60"))
	cfg := buildEngineConfig(poll, maxc, summary)
	runEngine(ctx, client.Pull, client.Ingest, safeProbe, cfg)
	log.Printf("culipulse-agent: shut down cleanly")
}

// buildDiscoveryReport is what this agent is configured to scan, reported on every pull so the console
// can show it. The server can't know any of this otherwise — it lives entirely in the agent's env,
// which is why this reads the same discovery/AWS env vars that main()'s goroutine-launch blocks read
// (to decide whether to start background discovery loops), independently of them.
func buildDiscoveryReport() *DiscoveryReport {
	report := &DiscoveryReport{}
	if cidr := os.Getenv("CULIPULSE_DISCOVER_CIDR"); cidr != "" {
		report.CIDR = cidr
		// Raw operator-set string, not parsePorts' parsed output, so the console shows what the
		// operator actually typed.
		report.Ports = os.Getenv("CULIPULSE_DISCOVER_PORTS")
	}
	if os.Getenv("CULIPULSE_AWS_SD") != "" {
		// Resolved list, not the raw env var, because this folds in the AWS_REGION fallback — the
		// console shows what is really being scanned.
		regions := parseRegions(os.Getenv("CULIPULSE_AWS_REGIONS"), os.Getenv("AWS_REGION"))
		report.AWSRegions = strings.Join(regions, ",")
	}
	return report
}

// buildEngineConfig derives the pipeline cadences from the poll interval and concurrency cap. Probes
// are dispatched on a fast tick (1s, or the poll interval if shorter) so they spread across each
// monitor's interval instead of bursting; results are batched for ingest to bound request size.
func buildEngineConfig(pollSeconds, maxc, summarySeconds int) engineConfig {
	pollInterval := time.Duration(pollSeconds) * time.Second
	dispatch := time.Second
	if pollInterval < dispatch {
		dispatch = pollInterval
	}
	summary := time.Duration(summarySeconds) * time.Second // <=0 disables the summary loop
	return engineConfig{
		maxConcurrency:   maxc,
		pollInterval:     pollInterval,
		dispatchInterval: dispatch,
		flushSize:        100,
		flushInterval:    5 * time.Second,
		ingestChunkSize:  100,
		ingestRetries:    3,
		summaryInterval:  summary,
	}
}

// everyUntil runs fn immediately, then every d, until ctx is cancelled. Replaces the old
// for{ fn(); time.Sleep(d) } pattern so the background discovery loops stop on shutdown.
func everyUntil(ctx context.Context, d time.Duration, fn func()) {
	fn()
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn()
		}
	}
}

// dueItems returns the work items whose own intervalSeconds has elapsed since they were last probed
// (or that have never been probed). intervalSeconds <= 0 is treated as always-due.
func dueItems(work []WorkItem, lastProbed map[string]int64, now int64) []WorkItem {
	out := make([]WorkItem, 0, len(work))
	for _, it := range work {
		last, seen := lastProbed[it.MonitorID]
		if !seen || it.IntervalSeconds <= 0 || now-last >= int64(it.IntervalSeconds) {
			out = append(out, it)
		}
	}
	return out
}
