// agent/timing_test.go
package main

import (
	"testing"
	"time"
)

func TestToDetail_FullHTTPS(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	pt := probeTiming{
		start: at(0), dnsStart: at(0), dnsDone: at(38),
		connectStart: at(38), connectDone: at(100),
		tlsStart: at(100), tlsDone: at(245),
		wroteRequest: at(245), firstByte: at(812), done: at(842),
	}
	d := pt.toDetail()
	if d["dnsMs"] != int64(38) || d["connectMs"] != int64(62) || d["tlsMs"] != int64(145) {
		t.Fatalf("setup phases wrong: %#v", d)
	}
	if d["waitMs"] != int64(567) || d["downloadMs"] != int64(30) || d["totalMs"] != int64(842) {
		t.Fatalf("transfer phases wrong: %#v", d)
	}
	if d["connReused"] != false {
		t.Fatalf("connReused want false: %#v", d)
	}
}

func TestToDetail_OmitsTLSForPlainHTTP(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	pt := probeTiming{start: at(0), dnsStart: at(0), dnsDone: at(10),
		connectStart: at(10), connectDone: at(20), wroteRequest: at(20), firstByte: at(100), done: at(110)}
	d := pt.toDetail()
	if _, ok := d["tlsMs"]; ok {
		t.Fatalf("plain http must omit tlsMs: %#v", d)
	}
}

func TestToDetail_ReusedConnOmitsSetup(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	pt := probeTiming{start: at(0), reused: true, wroteRequest: at(0), firstByte: at(50), done: at(55)}
	d := pt.toDetail()
	if d["connReused"] != true {
		t.Fatalf("want reused true: %#v", d)
	}
	for _, k := range []string{"dnsMs", "connectMs", "tlsMs"} {
		if _, ok := d[k]; ok {
			t.Fatalf("reused conn must omit %s: %#v", k, d)
		}
	}
	if d["waitMs"] != int64(50) || d["totalMs"] != int64(55) {
		t.Fatalf("reused transfer wrong: %#v", d)
	}
}

func TestToDetail_NilWhenNoResponse(t *testing.T) {
	pt := probeTiming{start: time.Unix(1, 0)} // never got first byte
	if pt.toDetail() != nil {
		t.Fatalf("want nil when no firstByte")
	}
}
