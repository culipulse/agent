package main

import "net/http"

// applyRequestSpec applies operator-configured request headers + auth to an outgoing probe.
// Only first-party agents ever receive decrypted auth (the worker strips it for tenant agents). G4.
func applyRequestSpec(req *http.Request, spec *Request) {
	if spec == nil {
		return
	}
	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}
	if spec.Auth != nil {
		switch spec.Auth.Type {
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+spec.Auth.Token)
		case "basic":
			req.SetBasicAuth(spec.Auth.Username, spec.Auth.Password)
		}
	}
	// Set Content-Type from spec only when the explicit headers map doesn't already supply one.
	// http.Header.Get is case-insensitive, so "content-type" from Headers is already covered.
	if spec.ContentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", spec.ContentType)
	}
}
