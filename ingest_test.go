package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func res(id string) IngestResult { return IngestResult{MonitorID: id, OK: true, TS: 1} }

func TestChunkResults(t *testing.T) {
	in := []IngestResult{res("a"), res("b"), res("c"), res("d"), res("e")}

	got := chunkResults(in, 2)
	if len(got) != 3 || len(got[0]) != 2 || len(got[1]) != 2 || len(got[2]) != 1 {
		t.Fatalf("chunk size 2 of 5 => [2,2,1], got lens %v", lens(got))
	}
	if got[2][0].MonitorID != "e" {
		t.Fatalf("last chunk should hold e, got %q", got[2][0].MonitorID)
	}

	// size >= len => single chunk
	if one := chunkResults(in, 10); len(one) != 1 || len(one[0]) != 5 {
		t.Fatalf("size>=len => one chunk of all, got lens %v", lens(one))
	}
	// size <= 0 => one chunk (defensive, never zero-size loop)
	if one := chunkResults(in, 0); len(one) != 1 || len(one[0]) != 5 {
		t.Fatalf("size<=0 => one chunk of all, got lens %v", lens(one))
	}
	// empty in => no chunks
	if z := chunkResults(nil, 3); len(z) != 0 {
		t.Fatalf("empty => no chunks, got %d", len(z))
	}
}

func lens(xs [][]IngestResult) []int {
	out := make([]int, len(xs))
	for i, x := range xs {
		out[i] = len(x)
	}
	return out
}

func TestIngestChunkedSplitsIntoBatches(t *testing.T) {
	var batches [][]IngestResult
	send := func(_ context.Context, r []IngestResult) error {
		batches = append(batches, r)
		return nil
	}
	in := []IngestResult{res("a"), res("b"), res("c")}
	if err := ingestChunked(context.Background(), send, in, 2, 0, noBackoff); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(batches) != 2 || len(batches[0]) != 2 || len(batches[1]) != 1 {
		t.Fatalf("expected batches [2,1], got %v", lens(batches))
	}
}

func TestIngestChunkedRetriesThenSucceeds(t *testing.T) {
	attempts := 0
	send := func(_ context.Context, _ []IngestResult) error {
		attempts++
		if attempts < 3 {
			return errors.New("transient")
		}
		return nil
	}
	if err := ingestChunked(context.Background(), send, []IngestResult{res("a")}, 100, 5, noBackoff); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestIngestChunkedReturnsErrorAfterExhausting(t *testing.T) {
	attempts := 0
	send := func(_ context.Context, _ []IngestResult) error {
		attempts++
		return errors.New("always fails")
	}
	err := ingestChunked(context.Background(), send, []IngestResult{res("a")}, 100, 2, noBackoff)
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if attempts != 3 { // initial + 2 retries
		t.Fatalf("expected 3 attempts (1 + 2 retries), got %d", attempts)
	}
}

func TestIngestChunkedStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	attempts := 0
	send := func(_ context.Context, _ []IngestResult) error {
		attempts++
		return errors.New("fail")
	}
	// backoff is consulted between retries; a cancelled ctx must abort before sleeping/retrying.
	err := ingestChunked(ctx, send, []IngestResult{res("a")}, 100, 5, func(int) time.Duration { return time.Hour })
	if err == nil {
		t.Fatal("expected context error")
	}
	if attempts > 1 {
		t.Fatalf("cancelled ctx should stop retrying, got %d attempts", attempts)
	}
}

func noBackoff(int) time.Duration { return 0 }
