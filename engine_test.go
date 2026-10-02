package main

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFormatStatsLine(t *testing.T) {
	got := formatStatsLine(5, 30*time.Second, 2, 10, 10, 1, 0)
	want := "status: 5 monitors | last 30s: 2 pulls, 10 probed, 10 ingested, 1 abstained, 0 ingest-errors"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *safeBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

// With a positive summaryInterval the engine periodically logs a one-line activity summary.
func TestEngineEmitsPeriodicSummary(t *testing.T) {
	var buf safeBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	probeFn := func(it WorkItem) IngestResult { return IngestResult{MonitorID: it.MonitorID, OK: true, TS: 1} }
	pull := func(context.Context) (*PullResponse, error) {
		return &PullResponse{Capabilities: []string{"http"}, Work: []WorkItem{{MonitorID: "m1", Type: "http", Target: "http://x", IntervalSeconds: 0}}}, nil
	}
	ingest := func(context.Context, []IngestResult) error { return nil }
	cfg := engineConfig{
		maxConcurrency: 2, pollInterval: 50 * time.Millisecond, dispatchInterval: 5 * time.Millisecond,
		flushSize: 10, flushInterval: 5 * time.Millisecond, ingestChunkSize: 100, ingestRetries: 1,
		summaryInterval: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runEngine(ctx, pull, ingest, probeFn, cfg); close(done) }()

	deadline := time.After(2 * time.Second)
	for !strings.Contains(buf.String(), "status:") {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("no summary line emitted; log=%q", buf.String())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestNormalizeIntervals(t *testing.T) {
	work := []WorkItem{{MonitorID: "a", IntervalSeconds: 60}, {MonitorID: "b", IntervalSeconds: 0}, {MonitorID: "c", IntervalSeconds: -5}}
	got := normalizeIntervals(work, 30)
	if got[0].IntervalSeconds != 60 {
		t.Errorf("positive interval must be preserved, got %d", got[0].IntervalSeconds)
	}
	if got[1].IntervalSeconds != 30 || got[2].IntervalSeconds != 30 {
		t.Errorf("non-positive intervals must fall back to poll(30), got %d and %d", got[1].IntervalSeconds, got[2].IntervalSeconds)
	}
	// must not mutate the caller's slice
	if work[1].IntervalSeconds != 0 {
		t.Errorf("input slice was mutated: %d", work[1].IntervalSeconds)
	}
	// poll<=0 defaults to 30 too
	if g := normalizeIntervals([]WorkItem{{MonitorID: "x", IntervalSeconds: 0}}, 0); g[0].IntervalSeconds != 30 {
		t.Errorf("poll<=0 should default to 30, got %d", g[0].IntervalSeconds)
	}
}

func TestPruneLastProbed(t *testing.T) {
	lastProbed := map[string]int64{"a": 1, "b": 2, "c": 3}
	pruneLastProbed(lastProbed, []WorkItem{{MonitorID: "a"}, {MonitorID: "c"}})
	if _, ok := lastProbed["b"]; ok {
		t.Error("b is no longer assigned and must be pruned")
	}
	if len(lastProbed) != 2 {
		t.Errorf("expected a and c to remain, got %v", lastProbed)
	}
}

func TestSplitDue(t *testing.T) {
	due := []WorkItem{{MonitorID: "h", Type: "http"}, {MonitorID: "t", Type: "tcp"}}
	supported, abstains := splitDue(due, []string{"http"})
	if len(supported) != 1 || supported[0].MonitorID != "h" {
		t.Fatalf("only the http item is supported, got %v", ids(supported))
	}
	if len(abstains) != 1 || abstains[0].MonitorID != "t" {
		t.Fatalf("the tcp item must abstain, got %d abstains", len(abstains))
	}
	if abstains[0].Cause == nil || *abstains[0].Cause != abstainCause {
		t.Fatalf("abstain must carry the abstain cause, got %v", abstains[0].Cause)
	}
}

// End-to-end: pull -> schedule -> worker(probe) -> batched ingest, plus the abstain path and a clean
// shutdown that flushes pending results when the context is cancelled.
func TestRunEngineProbesIngestsAbstainsAndShutsDown(t *testing.T) {
	var probed sync.Map
	probeFn := func(it WorkItem) IngestResult {
		probed.Store(it.MonitorID, true)
		return IngestResult{MonitorID: it.MonitorID, OK: true, TS: 1}
	}
	pull := func(context.Context) (*PullResponse, error) {
		return &PullResponse{Capabilities: []string{"http"}, Work: []WorkItem{
			{MonitorID: "m1", Type: "http", Target: "http://example", IntervalSeconds: 0},
			{MonitorID: "m2", Type: "tcp", Target: "example:1", IntervalSeconds: 0},
		}}, nil
	}
	var mu sync.Mutex
	ingested := map[string]IngestResult{}
	ingest := func(_ context.Context, rs []IngestResult) error {
		mu.Lock()
		for _, r := range rs {
			ingested[r.MonitorID] = r
		}
		mu.Unlock()
		return nil
	}
	cfg := engineConfig{
		maxConcurrency: 4, pollInterval: 50 * time.Millisecond, dispatchInterval: 5 * time.Millisecond,
		flushSize: 10, flushInterval: 5 * time.Millisecond, ingestChunkSize: 100, ingestRetries: 1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runEngine(ctx, pull, ingest, probeFn, cfg); close(done) }()

	// Wait until both the supported probe and the abstain have been ingested.
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		m1, ok1 := ingested["m1"]
		m2, ok2 := ingested["m2"]
		mu.Unlock()
		if ok1 && ok2 && m1.OK && m2.Cause != nil && *m2.Cause == abstainCause {
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatalf("timed out; ingested=%v", ingested)
		case <-time.After(3 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runEngine did not shut down after cancel")
	}

	if _, ok := probed.Load("m1"); !ok {
		t.Error("supported http monitor m1 should have been probed")
	}
	if _, ok := probed.Load("m2"); ok {
		t.Error("unsupported monitor m2 must NOT be probed (it abstains)")
	}
}

func TestEnqueueDiagnose(t *testing.T) {
	t.Run("queues a new item and marks it seen", func(t *testing.T) {
		seen := map[string]bool{}
		q := make(chan WorkItem, 4)
		resp := &PullResponse{Diagnose: []WorkItem{{MonitorID: "m1", RequestID: "req-1"}}}
		if d := enqueueDiagnose(resp, seen, q); d != 0 {
			t.Fatalf("dropped %d", d)
		}
		if len(q) != 1 || !seen["req-1"] {
			t.Fatalf("want 1 queued + seen, got len=%d seen=%v", len(q), seen)
		}
	})
	t.Run("skips seen and empty request ids", func(t *testing.T) {
		seen := map[string]bool{"req-1": true}
		q := make(chan WorkItem, 4)
		resp := &PullResponse{Diagnose: []WorkItem{{MonitorID: "m1", RequestID: "req-1"}, {MonitorID: "m2"}}}
		enqueueDiagnose(resp, seen, q)
		if len(q) != 0 {
			t.Fatalf("want nothing queued, got %d", len(q))
		}
	})
	t.Run("nil response is a no-op", func(t *testing.T) {
		if d := enqueueDiagnose(nil, map[string]bool{}, make(chan WorkItem, 1)); d != 0 {
			t.Fatal("nil resp must not drop")
		}
	})
	t.Run("full queue drops instead of blocking", func(t *testing.T) {
		q := make(chan WorkItem, 1)
		resp := &PullResponse{Diagnose: []WorkItem{{RequestID: "a"}, {RequestID: "b"}, {RequestID: "c"}}}
		done := make(chan int, 1)
		go func() { done <- enqueueDiagnose(resp, map[string]bool{}, q) }()
		select {
		case d := <-done:
			if d != 2 || len(q) != 1 {
				t.Fatalf("want 1 queued + 2 dropped, got queued=%d dropped=%d", len(q), d)
			}
		case <-time.After(time.Second):
			t.Fatal("enqueueDiagnose blocked on a full queue")
		}
	})
}

// The regression #313 is about: a slow diagnose must not delay delivery of the pulled work list.
func TestPullLoop_SlowDiagnoseDoesNotDelayUpdates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pull := func(context.Context) (*PullResponse, error) {
		return &PullResponse{Work: []WorkItem{{MonitorID: "w"}}, Diagnose: []WorkItem{{MonitorID: "m", RequestID: "r1"}}}, nil
	}
	q := make(chan WorkItem, diagnoseQueueSize)
	release := make(chan struct{})
	defer close(release)
	go diagnoseWorker(ctx, q, func(it WorkItem) IngestResult { <-release; return IngestResult{MonitorID: it.MonitorID} },
		func(context.Context, []IngestResult) error { return nil })
	updates := make(chan *PullResponse, 1)
	go pullLoop(ctx, pull, q, engineConfig{pollInterval: time.Hour}, updates, &engineStats{})
	select {
	case pr := <-updates:
		if len(pr.Work) != 1 {
			t.Fatalf("unexpected update %+v", pr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("work list was held back by an in-flight diagnose probe")
	}
}

// Diagnoses run concurrently across the pool and each result is ingested with its request id.
func TestDiagnoseWorkers_RunConcurrentlyAndIngest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q := make(chan WorkItem, diagnoseQueueSize)
	started := make(chan string, 2)
	release := make(chan struct{})
	var mu sync.Mutex
	var ingested []string
	ingestDone := make(chan struct{}, 2)
	probe := func(it WorkItem) IngestResult {
		started <- it.RequestID
		<-release
		return IngestResult{MonitorID: it.MonitorID, RequestID: it.RequestID}
	}
	ingest := func(_ context.Context, rs []IngestResult) error {
		mu.Lock()
		for _, r := range rs {
			ingested = append(ingested, r.RequestID)
		}
		mu.Unlock()
		ingestDone <- struct{}{}
		return nil
	}
	for i := 0; i < 2; i++ {
		go diagnoseWorker(ctx, q, probe, ingest)
	}
	q <- WorkItem{MonitorID: "m1", RequestID: "a"}
	q <- WorkItem{MonitorID: "m2", RequestID: "b"}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("two diagnoses did not run at the same time")
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		select {
		case <-ingestDone:
		case <-time.After(2 * time.Second):
			t.Fatal("result not ingested")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ingested) != 2 {
		t.Fatalf("want 2 ingested, got %v", ingested)
	}
}

func TestDiagnoseWorker_StopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		diagnoseWorker(ctx, make(chan WorkItem), func(WorkItem) IngestResult { return IngestResult{} },
			func(context.Context, []IngestResult) error { return nil })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not exit on cancel")
	}
}
