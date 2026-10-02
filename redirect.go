package main

import (
	"errors"
	"net/http"
	"strings"
)

// errInsecureRedirect: a check that carries secrets was redirected from https to plain http. We stop
// instead of sending the secrets in cleartext; classifyError maps it to cause "insecure_redirect".
var errInsecureRedirect = errors.New("insecure_redirect: refused https to http redirect for a check with secrets")

// redirectPolicy is the CheckRedirect for every HTTP probe (pooled and diagnose). Beyond the
// follow/10-hop rule it keeps a monitor's secrets on the host the operator configured (#312):
//   - once the chain reaches a host that is neither the original host nor a subdomain of it (the rule
//     net/http applies to Authorization), secret headers, Authorization and a secret body are dropped
//     for the rest of the chain — net/http re-copies the original headers on every hop, so this
//     re-checks the whole chain each time rather than only the latest hop;
//   - a hop from https to http is refused outright when the check carries any secret.
//
// Plain (non-secret) headers and bodies keep the standard net/http behaviour.
func redirectPolicy(item WorkItem) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !item.FollowRedirects || len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		spec := item.CheckSpec.Request
		if spec == nil || len(via) == 0 {
			return nil
		}
		hasSecret := len(spec.SecretHeaderNames) > 0 || spec.Auth != nil || spec.BodySecret
		if !hasSecret {
			return nil
		}
		if req.URL.Scheme == "http" {
			for _, v := range via {
				if v.URL.Scheme == "https" {
					return errInsecureRedirect
				}
			}
		}
		origin := strings.ToLower(via[0].URL.Hostname())
		left := !isSameOrSubdomain(strings.ToLower(req.URL.Hostname()), origin)
		for _, v := range via[1:] {
			if !isSameOrSubdomain(strings.ToLower(v.URL.Hostname()), origin) {
				left = true
			}
		}
		if !left {
			return nil
		}
		for _, name := range spec.SecretHeaderNames {
			req.Header.Del(name)
		}
		if spec.Auth != nil {
			req.Header.Del("Authorization")
		}
		if spec.BodySecret && req.Body != nil && req.Body != http.NoBody {
			_ = req.Body.Close()
			req.Body = http.NoBody
			req.GetBody = nil
			req.ContentLength = 0
		}
		return nil
	}
}

// isSameOrSubdomain mirrors net/http's isDomainOrSubdomain: sub equals parent, or ends in "."+parent.
// IPv6 literals never count as a subdomain.
func isSameOrSubdomain(sub, parent string) bool {
	if sub == parent {
		return true
	}
	if strings.ContainsAny(sub, ":%") {
		return false
	}
	return strings.HasSuffix(sub, "."+parent)
}
