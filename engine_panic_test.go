package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

// fakeTransport records events instead of sending them, so the capture path is testable offline.
type fakeTransport struct{ events []*sentry.Event }

func (t *fakeTransport) Configure(sentry.ClientOptions)        {}
func (t *fakeTransport) SendEvent(e *sentry.Event)             { t.events = append(t.events, e) }
func (t *fakeTransport) Flush(time.Duration) bool              { return true }
func (t *fakeTransport) FlushWithContext(context.Context) bool { return true }
func (t *fakeTransport) Close()                                {}

func TestCaptureLoopPanicSendsTaggedEvent(t *testing.T) {
	ft := &fakeTransport{}
	if err := sentry.Init(sentry.ClientOptions{Dsn: "https://test@example.com/1", Transport: ft}); err != nil {
		t.Fatal(err)
	}
	defer sentry.CurrentHub().BindClient(nil) // reset global hub (unbind the client)

	captureLoopPanic("pull", "boom in loop")
	sentry.Flush(time.Second)

	if len(ft.events) != 1 {
		t.Fatalf("expected 1 captured event, got %d", len(ft.events))
	}
	if got := ft.events[0].Tags["loop"]; got != "pull" {
		t.Fatalf("loop tag = %q, want \"pull\"", got)
	}
	if ft.events[0].Tags["version"] == "" || ft.events[0].Tags["arch"] == "" {
		t.Fatalf("expected version+arch tags, got %v", ft.events[0].Tags)
	}
}

// TestCaptureLoopPanicNoopWhenDisabled proves the capture call is a safe no-op with no Sentry client
// (the empty-DSN default): it must not panic.
func TestCaptureLoopPanicNoopWhenDisabled(t *testing.T) {
	sentry.CurrentHub().BindClient(nil) // no client => disabled hub
	captureLoopPanic("pull", "boom")    // must not panic
}

// TestSafeGoRecoversPanic proves safeGo recovers a panic in the launched goroutine instead of
// crashing the process — the failure mode a bare `go fn()` main-loop launch has today.
func TestSafeGoRecoversPanic(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(1)
	// safeGo must recover the panic so the test process does not crash.
	safeGo("panicker", func() {
		defer wg.Done()
		panic("boom in loop")
	})
	wg.Wait() // if safeGo did not recover, the process would have crashed
}

// TestRestartOnPanicReentersAfterPanic proves the restart semantics required for the agent's
// long-running main loops (pull/schedule/ingest/stats): after a recovered panic, restartOnPanic
// re-invokes fn instead of letting the goroutine die, so the loop keeps running for the life of the
// process. It stops restarting once fn returns normally (the clean-shutdown case).
func TestRestartOnPanicReentersAfterPanic(t *testing.T) {
	orig := restartBackoff
	restartBackoff = time.Millisecond
	defer func() { restartBackoff = orig }()

	var calls int
	restartOnPanic("test-loop", func() {
		calls++
		if calls < 3 {
			panic("simulated crash")
		}
		// 3rd call returns normally: restartOnPanic must stop here, not loop forever.
	})
	if calls != 3 {
		t.Fatalf("expected fn to be invoked 3 times (2 recovered panics + 1 clean return), got %d", calls)
	}
}

func TestRestartOnPanicExitsAfterTooManyPanics(t *testing.T) {
	origBackoff := restartBackoff
	restartBackoff = time.Millisecond
	defer func() { restartBackoff = origBackoff }()
	origMax := maxRestartsInWindow
	maxRestartsInWindow = 3
	defer func() { maxRestartsInWindow = origMax }()
	origWin := restartWindow
	restartWindow = time.Minute
	defer func() { restartWindow = origWin }()
	origExit := exitProcess
	var exited int
	exitProcess = func(int) { exited++ } // record instead of exiting
	defer func() { exitProcess = origExit }()

	var calls int
	restartOnPanic("always-panics", func() {
		calls++
		panic("deterministic crash")
	})

	if exited != 1 {
		t.Fatalf("expected exitProcess to fire exactly once, got %d", exited)
	}
	// maxRestartsInWindow=3 => panics 1..3 stay under the cap, the 4th exceeds it and triggers exit.
	if calls != 4 {
		t.Fatalf("expected fn invoked 4 times before exit, got %d", calls)
	}
}
