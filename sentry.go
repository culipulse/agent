package main

import (
	"log"
	"os"
	"regexp"

	"github.com/getsentry/sentry-go"
)

// initSentry turns on error telemetry ONLY when SENTRY_DSN is set (first-party boxes inject it as a
// runtime env var, exactly like CULIPULSE_AGENT_TOKEN). Empty DSN => no init, no network, no behavior
// change — the tenant/public default. The DSN is a write key and MUST NOT be baked into the binary
// (the image + KV binaries are a public mirror). Errors only (TracesSampleRate 0); Release is the
// ldflags-stamped Version. Returns whether Sentry was enabled so main() can defer a Flush.
func initSentry() bool {
	dsn := os.Getenv("SENTRY_DSN")
	if dsn == "" {
		return false
	}
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Release:          Version,
		TracesSampleRate: 0,
		Environment:      os.Getenv("CULIPULSE_AGENT_ENV"), // "" is fine (Sentry defaults it)
		BeforeSend:       scrubEvent,
	})
	if err != nil {
		log.Printf("sentry init failed (continuing without telemetry): %v", err)
		return false
	}
	return true
}

var urlQueryRe = regexp.MustCompile(`(https?://[^\s?]+)\?[^\s]*`)

// scrubMessage strips query strings from any URLs in s (they can carry tokens), keeping scheme+host+path.
func scrubMessage(s string) string {
	return urlQueryRe.ReplaceAllString(s, "$1")
}

// scrubEvent is the Sentry BeforeSend hook: applies scrubMessage to the event message and every
// exception value. Total — never nils out the event. (Tenant panics never reach here — no DSN — but
// first-party probe panics could embed a target URL.)
func scrubEvent(event *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	event.Message = scrubMessage(event.Message)
	for i := range event.Exception {
		event.Exception[i].Value = scrubMessage(event.Exception[i].Value)
	}
	return event
}
