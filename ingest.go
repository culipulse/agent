package main

import (
	"context"
	"time"
)

// ingestFunc sends one batch of results (Client.Ingest fits this signature).
type ingestFunc func(ctx context.Context, results []IngestResult) error

// chunkResults splits results into batches of at most size. A whole cycle's results are POSTed to the
// worker, which processes each result with its own DB + Durable-Object round-trip; an unbounded batch
// risks the per-request subrequest ceiling and the agent's ingest timeout, losing the entire batch.
// Chunking bounds each request so a large wave degrades gracefully instead of all-or-nothing.
func chunkResults(results []IngestResult, size int) [][]IngestResult {
	if len(results) == 0 {
		return nil
	}
	if size <= 0 {
		size = len(results)
	}
	out := make([][]IngestResult, 0, (len(results)+size-1)/size)
	for i := 0; i < len(results); i += size {
		end := i + size
		if end > len(results) {
			end = len(results)
		}
		out = append(out, results[i:end])
	}
	return out
}

// ingestChunked splits results into batches and sends each with bounded retry/backoff. A failed batch
// is retried up to maxRetries times; backoff(attempt) is the wait before retry attempt N (attempt>=1).
// A cancelled context aborts before the next retry. Returns the first batch error that survives retry.
func ingestChunked(ctx context.Context, send ingestFunc, results []IngestResult, chunkSize, maxRetries int, backoff func(attempt int) time.Duration) error {
	for _, batch := range chunkResults(results, chunkSize) {
		if err := sendWithRetry(ctx, send, batch, maxRetries, backoff); err != nil {
			return err
		}
	}
	return nil
}

func sendWithRetry(ctx context.Context, send ingestFunc, batch []IngestResult, maxRetries int, backoff func(attempt int) time.Duration) error {
	var err error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff(attempt)):
			}
		}
		if err = send(ctx, batch); err == nil {
			return nil
		}
	}
	return err
}

// expBackoff is the default retry backoff: 200ms, 400ms, 800ms, ... capped at 5s.
func expBackoff(attempt int) time.Duration {
	d := 200 * time.Millisecond << (attempt - 1)
	if d > 5*time.Second {
		return 5 * time.Second
	}
	return d
}
