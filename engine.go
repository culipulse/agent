package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

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

// runEngine runs the probe pipeline until ctx is cancelled, then drains in-flight work and flushes
// pending results. Pull, scheduling, probing, and ingest are decoupled goroutines connected by
// channels, so a slow probe wave or a slow ingest never blocks the next pull, and probes are spread
// across the dispatch ticks instead of firing in one synchronized burst per poll.
// dispatchDiagnose probes each not-yet-seen diagnose one-shot exactly once (fresh connection via the
// supplied probe) and returns the tagged results. `seen` is mutated to remember served request ids, so
// a request still 'dispatched' on a later pull is not re-probed.
func dispatchDiagnose(resp *PullResponse, seen map[string]bool, probe func(WorkItem) IngestResult) []IngestResult {
	if resp == nil {
		return nil
	}
	out := make([]IngestResult, 0, len(resp.Diagnose))
	for _, it := range resp.Diagnose {
		if it.RequestID == "" || seen[it.RequestID] {
			continue
		}
		seen[it.RequestID] = true
		out = append(out, probe(it))
	}
	return out
}

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

	// Ingester: batches results and POSTs them, decoupled from probing.
	var ingester sync.WaitGroup
	ingester.Add(1)
	go func() {
		defer ingester.Done()
		ingestLoop(ingest, results, cfg, stats)
	}()

	// Pull: refresh the work list on its own cadence, never blocked by probing/ingest.
	// Diagnose items are probed inline in pullLoop (bypassing the shared jobs/results channels to avoid
	// send-on-closed-channel races) using probeDiagnose for fresh-connection one-shot probes.
	go pullLoop(ctx, pull, ingest, probeDiagnose, cfg, updates, stats)

	// Optional periodic activity summary (operator visibility).
	if cfg.summaryInterval > 0 {
		go statsLoop(ctx, stats, cfg.summaryInterval)
	}

	// Scheduler runs in this goroutine, so runEngine blocks until ctx is cancelled.
	scheduleLoop(ctx, cfg, updates, jobs, results, stats) // sole sender to jobs; closes it on return

	workers.Wait()  // workers drain remaining jobs and exit
	close(results)  // safe: scheduler (abstains) returned and all workers are done
	ingester.Wait() // ingester performs its final flush and exits
}

func pullLoop(ctx context.Context, pull func(context.Context) (*PullResponse, error), ingest ingestFunc, diagProbe func(WorkItem) IngestResult, cfg engineConfig, updates chan<- *PullResponse, stats *engineStats) {
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
		// Probe diagnose items inline (fresh connection, one-shot, deduped) and POST directly via
		// ingest — bypasses the shared jobs/results channels entirely so there is no channel-close race.
		if diag := dispatchDiagnose(pr, seen, diagProbe); len(diag) > 0 {
			if err := ingest(cctx, diag); err != nil {
				log.Printf("diagnose ingest error: %v", err)
			}
		}
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

func scheduleLoop(ctx context.Context, cfg engineConfig, updates <-chan *PullResponse, jobs chan<- WorkItem, results chan<- IngestResult, stats *engineStats) {
	defer close(jobs) // scheduler is the only sender to jobs
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
