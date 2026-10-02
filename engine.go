package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
)

// captureLoopPanic reports a recovered main-loop panic to Sentry, tagged so issues group per loop.
// Total + safe: when Sentry is not initialized (empty SENTRY_DSN — the default), the current hub has
// no client and Recover no-ops, so this makes no network calls and never affects the restart path.
func captureLoopPanic(name string, r any) {
	// Clone the hub: the main loops run in separate goroutines, and the global hub has a single shared
	// scope stack — a clone gives this capture its own scope (sharing the client) so concurrent panics
	// in two loops can't cross-tag each other's events. This is sentry-go's documented goroutine pattern.
	hub := sentry.CurrentHub().Clone()
	hub.WithScope(func(scope *sentry.Scope) {
		scope.SetTag("loop", name)
		scope.SetTag("arch", runtime.GOARCH)
		scope.SetTag("version", Version)
		if reg := os.Getenv("CULIPULSE_AGENT_REGION"); reg != "" {
			scope.SetTag("region", reg)
		}
		hub.Recover(r)
	})
}

// engineStats holds cumulative pipeline counters, incremented from the pull/schedule/ingest
// goroutines and snapshotted by the summary loop to log periodic activity.
type engineStats struct {
	pulls      atomic.Int64
	dispatched atomic.Int64
	abstained  atomic.Int64
	ingested   atomic.Int64
	ingestErr  atomic.Int64
	work       atomic.Int64 // last-seen work-list size (set, not added)
}

type statsSnapshot struct{ pulls, dispatched, ingested, abstained, ingestErr, work int64 }

func (s *engineStats) snap() statsSnapshot {
	return statsSnapshot{
		pulls: s.pulls.Load(), dispatched: s.dispatched.Load(), ingested: s.ingested.Load(),
		abstained: s.abstained.Load(), ingestErr: s.ingestErr.Load(), work: s.work.Load(),
	}
}

// formatStatsLine renders a one-line summary of activity over the window (deltas already computed).
func formatStatsLine(monitors int64, window time.Duration, pulls, probed, ingested, abstained, ingestErr int64) string {
	return fmt.Sprintf("status: %d monitors | last %s: %d pulls, %d probed, %d ingested, %d abstained, %d ingest-errors",
		monitors, window, pulls, probed, ingested, abstained, ingestErr)
}

// probeFunc runs one probe (safeProbe in production; a stub in tests).
type probeFunc func(WorkItem) IngestResult

// engineConfig holds the cadences and bounds for the probe pipeline.
type engineConfig struct {
	maxConcurrency   int           // worker pool size (max simultaneous probes)
	pollInterval     time.Duration // how often to pull the work list
	dispatchInterval time.Duration // how often to enqueue due monitors (smooths the wave)
	flushSize        int           // ingest when this many results have accumulated
	flushInterval    time.Duration // ...or this long has elapsed, whichever first
	ingestChunkSize  int           // max results per ingest POST
	ingestRetries    int           // retries per ingest batch
	summaryInterval  time.Duration // how often to log an activity summary (0 disables it)
}

const (
	enginePullTimeout   = 60 * time.Second
	engineIngestTimeout = 30 * time.Second
)

// normalizeIntervals returns a copy of work with non-positive intervals replaced by the poll cadence,
// so the fast dispatch tick never probes an interval<=0 monitor more often than the old poll loop did.
func normalizeIntervals(work []WorkItem, pollSeconds int) []WorkItem {
	if pollSeconds <= 0 {
		pollSeconds = 30
	}
	out := make([]WorkItem, len(work))
	copy(out, work)
	for i := range out {
		if out[i].IntervalSeconds <= 0 {
			out[i].IntervalSeconds = pollSeconds
		}
	}
	return out
}

// pruneLastProbed drops scheduling state for monitors no longer in the work list (keeps the map bounded).
func pruneLastProbed(lastProbed map[string]int64, work []WorkItem) {
	live := make(map[string]struct{}, len(work))
	for _, it := range work {
		live[it.MonitorID] = struct{}{}
	}
	for id := range lastProbed {
		if _, ok := live[id]; !ok {
			delete(lastProbed, id)
		}
	}
}

// splitDue partitions due work into items this agent can probe and explicit abstains (a non-vote) for
// items whose capability it lacks — surfaced instead of silently dropped (G5).
func splitDue(due []WorkItem, caps []string) (supported []WorkItem, abstains []IngestResult) {
	for _, it := range due {
		if supports(caps, it) {
			supported = append(supported, it)
			continue
		}
		log.Printf("abstain monitor=%s type=%s: agent lacks %q capability", it.MonitorID, it.Type, requiredCap(it))
		abstains = append(abstains, unsupportedResult(it))
	}
	return supported, abstains
}

const (
	diagnoseWorkers   = 4
	diagnoseQueueSize = 32
)

// enqueueDiagnose hands each not-yet-seen diagnose one-shot to the diagnose pool without blocking.
// `seen` remembers served request ids so one still 'dispatched' on a later pull is not re-probed.
// When the queue is full the item is dropped and logged: the server already marked it dispatched, so
// it simply expires — the queue (32) far exceeds real diagnose volume, so this should be rare; a
// server-side rate limit on diagnose requests is tracked separately.
func enqueueDiagnose(resp *PullResponse, seen map[string]bool, queue chan<- WorkItem) (dropped int) {
	if resp == nil {
		return 0
	}
	for _, it := range resp.Diagnose {
		if it.RequestID == "" || seen[it.RequestID] {
			continue
		}
		seen[it.RequestID] = true
		select {
		case queue <- it:
		default:
			dropped++
			log.Printf("diagnose queue full, dropping request=%s monitor=%s", it.RequestID, it.MonitorID)
		}
	}
	return dropped
}

// diagnoseWorker probes queued diagnose one-shots (fresh connection via probe) and POSTs each result
// straight to ingest, bypassing the shared jobs/results channels so there is no channel-close race.
func diagnoseWorker(ctx context.Context, queue <-chan WorkItem, probe func(WorkItem) IngestResult, ingest ingestFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		case it := <-queue:
			res := probe(it)
			ictx, cancel := context.WithTimeout(ctx, enginePullTimeout)
			if err := ingest(ictx, []IngestResult{res}); err != nil {
				log.Printf("diagnose ingest error: %v", err)
			}
			cancel()
		}
	}
}

// runRecovered runs fn under panic recovery: a panic is logged (name-tagged, with the panic value and
// stack) instead of crashing the process. Returns true if fn panicked (and was recovered), false if fn
// returned normally — callers that need to keep a loop alive use this to decide whether to restart.
func runRecovered(name string, fn func()) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("goroutine panic recovered name=%s: %v\n%s", name, r, debug.Stack())
			captureLoopPanic(name, r)
			panicked = true
		}
	}()
	fn()
	return false
}

// safeGo runs fn once in a new goroutine under panic recovery (see runRecovered): a panic in fn is
// logged and does not crash the process, but fn is not restarted. Use for goroutines that are meant to
// run to completion once (e.g. a one-shot task), not for a main loop that must keep running.
func safeGo(name string, fn func()) {
	go runRecovered(name, fn)
}

// restartBackoff bounds how often a panicking main loop is restarted, so a deterministic panic (e.g. a
// bug hit on every tick) can't spin the CPU in a tight crash loop. A var (not const) so tests can
// shrink it instead of waiting out the real delay.
var restartBackoff = time.Second

// A deterministic panic (same input crashes every tick) would otherwise spin restartOnPanic forever
// at restartBackoff, leaving the process "up" to its supervisor while doing no probing — harder to
// notice than a clean crash. maxRestartsInWindow/restartWindow bound that: exceeding the cap exits the
// process so docker's --restart=unless-stopped restarts it fresh (and the per-panic Sentry report has
// already fired). Vars, not consts, so tests can shrink them. exitProcess is overridable for tests.
var (
	maxRestartsInWindow = 5
	restartWindow       = time.Minute
	exitProcess         = os.Exit
)

// restartOnPanic runs fn, and if fn panics, recovers, logs (via runRecovered), waits restartBackoff,
// and re-invokes fn — so a main-loop goroutine (pull/schedule/ingest/stats) keeps running after a
// recovered panic instead of silently going quiet for the rest of the process's life, which would be
// worse than a clean crash (the agent looks alive but stops doing its job). fn must return only on a
// clean shutdown (e.g. ctx.Done()); a normal return stops the retries for good. Runs synchronously in
// the calling goroutine — callers that want this in its own goroutine use safeLoop.
func restartOnPanic(name string, fn func()) {
	var panics []time.Time
	for {
		if !runRecovered(name, fn) {
			return // clean return (e.g. ctx.Done) — stop restarting for good
		}
		now := time.Now()
		cutoff := now.Add(-restartWindow)
		kept := panics[:0]
		for _, ts := range panics {
			if ts.After(cutoff) {
				kept = append(kept, ts)
			}
		}
		panics = append(kept, now)
		if len(panics) > maxRestartsInWindow {
			log.Printf("goroutine %q panicked %d times within %s — exiting for a clean supervisor restart instead of a silent restart loop", name, len(panics), restartWindow)
			sentry.Flush(2 * time.Second) // best-effort; no-op when Sentry disabled
			exitProcess(1)
			return // reached only in tests where exitProcess doesn't exit; avoids spinning
		}
		time.Sleep(restartBackoff)
	}
}

// safeLoop is restartOnPanic launched in its own goroutine — the long-running-loop counterpart to
// safeGo: a panic is recovered, logged, and the loop is restarted (after restartBackoff) rather than
// left dead for the rest of the process's life.
func safeLoop(name string, fn func()) {
	go restartOnPanic(name, fn)
}

// runEngine runs the probe pipeline until ctx is cancelled. On shutdown it drains in-flight probe/ingest
// work and flushes pending results, but diagnose one-shots still in flight are dropped rather than
// waited on — the diagnose pool is tied directly to ctx and returns as soon as it is cancelled (see
// diagnoseWorker). Pull, scheduling, probing, and ingest are decoupled goroutines connected by channels,
// so a slow probe wave or a slow ingest never blocks the next pull, and probes are spread across the
// dispatch ticks instead of firing in one synchronized burst per poll.
// Diagnose one-shots run on their own small pool so a slow diagnose never delays the pulled work list
// (#313): before, they ran serially inside pullLoop ahead of `updates <- pr`, so N slow targets held
// back new/edited/paused monitors for every tenant on the agent by ~N × timeout.
func runEngine(ctx context.Context, pull func(context.Context) (*PullResponse, error), ingest ingestFunc, probeFn probeFunc, cfg engineConfig) {
	jobs := make(chan WorkItem, cfg.maxConcurrency)
	results := make(chan IngestResult, cfg.maxConcurrency*2)
	updates := make(chan *PullResponse, 1)
	stats := &engineStats{}

	// Worker pool: bounded simultaneous probes.
	var workers sync.WaitGroup
	workers.Add(cfg.maxConcurrency)
	for i := 0; i < cfg.maxConcurrency; i++ {
		go func() {
			defer workers.Done()
			for item := range jobs {
				results <- probeFn(item)
			}
		}()
	}

	// Ingester: batches results and POSTs them, decoupled from probing. restartOnPanic keeps it
	// pulling from `results` after a recovered panic instead of stalling ingest for good while the
	// process stays alive (see runRecovered/restartOnPanic).
	var ingester sync.WaitGroup
	ingester.Add(1)
	go func() {
		defer ingester.Done()
		restartOnPanic("ingest", func() { ingestLoop(ingest, results, cfg, stats) })
	}()

	// Diagnose pool: a few workers fed by a bounded queue, started once here (not inside pullLoop) so a
	// pullLoop restart never spawns a second pool. Each worker is a long-running loop restarted on panic.
	diagQueue := make(chan WorkItem, diagnoseQueueSize)
	for i := 0; i < diagnoseWorkers; i++ {
		safeLoop("diagnose", func() { diagnoseWorker(ctx, diagQueue, probeDiagnose, ingest) })
	}

	// Pull: refresh the work list on its own cadence, never blocked by probing/ingest/diagnose.
	safeLoop("pull", func() { pullLoop(ctx, pull, diagQueue, cfg, updates, stats) })

	// Optional periodic activity summary (operator visibility). Long-running for{} loop; restart on
	// panic like the other main loops.
	if cfg.summaryInterval > 0 {
		safeLoop("stats", func() { statsLoop(ctx, stats, cfg.summaryInterval) })
	}

	// Scheduler runs in this goroutine, so runEngine blocks until ctx is cancelled. restartOnPanic
	// keeps scheduling alive across a recovered panic; jobs is closed exactly once here (not inside
	// scheduleLoop) so a restart never double-closes it.
	restartOnPanic("schedule", func() {
		scheduleLoop(ctx, cfg, updates, jobs, results, stats)
	})
	close(jobs) // sole sender to jobs; closed once scheduleLoop returns cleanly (ctx.Done())

	workers.Wait()  // workers drain remaining jobs and exit
	close(results)  // safe: scheduler (abstains) returned and all workers are done
	ingester.Wait() // ingester performs its final flush and exits
}

func pullLoop(ctx context.Context, pull func(context.Context) (*PullResponse, error), diagQueue chan<- WorkItem, cfg engineConfig, updates chan<- *PullResponse, stats *engineStats) {
	seen := map[string]bool{} // persists across pulls; deduplicates diagnose request ids
	doPull := func() {
		cctx, cancel := context.WithTimeout(ctx, enginePullTimeout)
		defer cancel()
		pr, err := pull(cctx)
		if err != nil {
			log.Printf("pull error: %v", err)
			return
		}
		stats.pulls.Add(1)
		stats.work.Store(int64(len(pr.Work)))
		enqueueDiagnose(pr, seen, diagQueue) // never blocks: the work list below goes out right away
		select {
		case updates <- pr:
		case <-ctx.Done():
		}
	}
	doPull() // immediate first pull so probing starts without waiting a full interval
	t := time.NewTicker(cfg.pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			doPull()
		}
	}
}

// scheduleLoop does not close jobs itself (a restart via restartOnPanic would double-close it) — the
// caller in runEngine closes jobs exactly once, after scheduleLoop returns cleanly on ctx.Done().
func scheduleLoop(ctx context.Context, cfg engineConfig, updates <-chan *PullResponse, jobs chan<- WorkItem, results chan<- IngestResult, stats *engineStats) {
	var work []WorkItem
	var caps []string
	lastProbed := map[string]int64{}
	pollSeconds := int(cfg.pollInterval / time.Second)
	d := time.NewTicker(cfg.dispatchInterval)
	defer d.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case pr := <-updates:
			if pr == nil {
				continue
			}
			work = normalizeIntervals(pr.Work, pollSeconds)
			caps = pr.Capabilities
			pruneLastProbed(lastProbed, work)
		case <-d.C:
			now := time.Now().Unix()
			supported, abstains := splitDue(dueItems(work, lastProbed, now), caps)
			// Non-blocking enqueue: if the worker pool is saturated, leave the monitor "due" and retry
			// next tick. This bounds in-flight probes and naturally spreads a large backlog over time.
			for _, it := range supported {
				select {
				case jobs <- it:
					lastProbed[it.MonitorID] = now
					stats.dispatched.Add(1)
				default:
				}
			}
			for _, ab := range abstains {
				select {
				case results <- ab:
					lastProbed[ab.MonitorID] = now
					stats.abstained.Add(1)
				default:
				}
			}
		}
	}
}

// statsLoop logs a one-line activity summary every interval until ctx is cancelled, so a healthy
// agent isn't silent — the decoupled pipeline has no per-cycle log of its own.
func statsLoop(ctx context.Context, stats *engineStats, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	prev := stats.snap()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := stats.snap()
			log.Print(formatStatsLine(cur.work, interval,
				cur.pulls-prev.pulls, cur.dispatched-prev.dispatched, cur.ingested-prev.ingested,
				cur.abstained-prev.abstained, cur.ingestErr-prev.ingestErr))
			prev = cur
		}
	}
}

func ingestLoop(ingest ingestFunc, results <-chan IngestResult, cfg engineConfig, stats *engineStats) {
	batch := make([]IngestResult, 0, cfg.flushSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		n := len(batch)
		// Use a fresh context (not the engine's) so a flush triggered during shutdown still delivers.
		ictx, cancel := context.WithTimeout(context.Background(), engineIngestTimeout)
		defer cancel()
		if err := ingestChunked(ictx, ingest, batch, cfg.ingestChunkSize, cfg.ingestRetries, expBackoff); err != nil {
			log.Printf("ingest error (%d results dropped): %v", n, err)
			stats.ingestErr.Add(1)
		} else {
			stats.ingested.Add(int64(n))
		}
		batch = batch[:0]
	}
	t := time.NewTicker(cfg.flushInterval)
	defer t.Stop()
	for {
		select {
		case r, ok := <-results:
			if !ok {
				flush() // channel closed on shutdown: final flush, then exit
				return
			}
			batch = append(batch, r)
			if len(batch) >= cfg.flushSize {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}
