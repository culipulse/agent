package main

import (
	"strings"
	"testing"

	"github.com/getsentry/sentry-go"
)

func TestInitSentryDisabledWhenNoDSN(t *testing.T) {
	t.Setenv("SENTRY_DSN", "")
	sentry.CurrentHub().BindClient(nil)       // clean slate — no client, so we test initSentry alone
	defer sentry.CurrentHub().BindClient(nil) // don't leak state to other tests
	if initSentry() {
		t.Fatal("initSentry() = true with empty SENTRY_DSN; want false (disabled)")
	}
	if sentry.CurrentHub().Client() != nil {
		t.Fatal("initSentry created a Sentry client despite empty SENTRY_DSN")
	}
}

func TestInitSentryEnabledWhenDSN(t *testing.T) {
	t.Setenv("SENTRY_DSN", "https://test@example.com/1")
	defer sentry.CurrentHub().BindClient(nil) // reset: unbind the client for other tests
	if !initSentry() {
		t.Fatal("initSentry() = false with a valid SENTRY_DSN; want true")
	}
	if sentry.CurrentHub().Client() == nil {
		t.Fatal("no Sentry client created despite a valid SENTRY_DSN")
	}
}

func TestScrubMessageRedactsURLQuery(t *testing.T) {
	got := scrubMessage("dial https://api.example.com/p?token=secret&x=1 failed")
	if strings.Contains(got, "token=secret") || strings.Contains(got, "x=1") {
		t.Fatalf("query string not redacted: %q", got)
	}
	if !strings.Contains(got, "https://api.example.com/p") {
		t.Fatalf("scheme+host+path should survive: %q", got)
	}
}
