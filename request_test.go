package main

import (
	"net/http"
	"testing"
)

func TestApplyRequestSpecBearer(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://x", nil)
	applyRequestSpec(req, &Request{Auth: &Auth{Type: "bearer", Token: "tok"}})
	if got := req.Header.Get("Authorization"); got != "Bearer tok" {
		t.Fatalf("Authorization = %q, want Bearer tok", got)
	}
}

func TestApplyRequestSpecBasic(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://x", nil)
	applyRequestSpec(req, &Request{Auth: &Auth{Type: "basic", Username: "u", Password: "p"}})
	user, pass, ok := req.BasicAuth()
	if !ok || user != "u" || pass != "p" {
		t.Fatalf("BasicAuth = %q/%q ok=%v, want u/p", user, pass, ok)
	}
}

func TestApplyRequestSpecHeadersAndNil(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://x", nil)
	applyRequestSpec(req, &Request{Headers: map[string]string{"X-Env": "prod"}})
	if got := req.Header.Get("X-Env"); got != "prod" {
		t.Fatalf("X-Env = %q, want prod", got)
	}
	applyRequestSpec(req, nil) // must not panic
}
