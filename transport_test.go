package main

import (
	"encoding/hex"
	"testing"
)

// The console's "DNS resolver" preset expects the reply header aaaa8180 (same id, QR+RD+RA, rcode 0)
// so a REFUSED or SERVFAIL answer is not read as up (#387). Reply headers below were captured live:
// 8.8.8.8 / 1.1.1.1 / 9.9.9.9 answer example.com with aaaa8180, ns1.google.com (not recursive for
// example.com) with aaaa8105 (REFUSED), and dnssec-failed.org via 8.8.8.8 with aaaa8182 (SERVFAIL).
func TestMatchReplyDNSResolverPreset(t *testing.T) {
	tr := &Transport{PayloadEncoding: "hex", Expect: "aaaa8180", ExpectMode: "contains"}
	cases := []struct {
		name   string
		header string
		want   bool
	}{
		{"noerror from a resolver", "aaaa81800001000200000000", true},
		{"refused", "aaaa81050001000000000000", false},
		{"servfail", "aaaa81820001000000000000", false},
		{"nxdomain", "aaaa81830001000000000000", false},
	}
	for _, c := range cases {
		reply, err := hex.DecodeString(c.header)
		if err != nil {
			t.Fatal(err)
		}
		got, err := matchReply(tr, reply)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: matchReply = %v, want %v", c.name, got, c.want)
		}
	}
}
