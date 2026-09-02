package main

import (
	"math"
	"testing"
	"time"
)

func ms(n float64) time.Duration { return time.Duration(n * float64(time.Millisecond)) }

func TestComputePingStats_AllReceived(t *testing.T) {
	st := computePingStats(4, []time.Duration{ms(10), ms(20), ms(30), ms(40)})
	if st.Sent != 4 || st.Received != 4 || st.LossPct != 0 {
		t.Fatalf("counts: %+v", st)
	}
	if st.RttMinMs != 10 || st.RttMaxMs != 40 || st.RttAvgMs != 25 {
		t.Fatalf("min/avg/max: %+v", st)
	}
	// stddev of 10,20,30,40 (population) = sqrt(125) ≈ 11.18
	if math.Abs(st.RttStddevMs-math.Sqrt(125)) > 0.01 {
		t.Fatalf("stddev: %v", st.RttStddevMs)
	}
}

func TestComputePingStats_PartialLoss(t *testing.T) {
	st := computePingStats(4, []time.Duration{ms(10), ms(30)}) // 2 of 4 lost
	if st.Received != 2 || st.LossPct != 50 {
		t.Fatalf("loss: %+v", st)
	}
	if st.RttAvgMs != 20 {
		t.Fatalf("avg over received only: %v", st.RttAvgMs)
	}
}

func TestComputePingStats_TotalLoss(t *testing.T) {
	st := computePingStats(4, nil)
	if st.Received != 0 || st.LossPct != 100 || st.RttAvgMs != 0 {
		t.Fatalf("total loss: %+v", st)
	}
}

func TestPingCount_DefaultAndClamp(t *testing.T) {
	if pingCount(nil) != 4 {
		t.Fatal("nil ⇒ default 4")
	}
	if pingCount(&Ping{Count: 0}) != 4 {
		t.Fatal("0 ⇒ default 4")
	}
	if pingCount(&Ping{Count: 99}) != 10 {
		t.Fatal("clamp high to 10")
	}
	if pingCount(&Ping{Count: 1}) != 1 {
		t.Fatal("1 stays 1")
	}
}

func TestPingDegraded(t *testing.T) {
	st := PingStats{Received: 4, LossPct: 50, RttAvgMs: 100}
	if pingDegraded(st, nil) {
		t.Fatal("no config ⇒ never degraded")
	}
	if !pingDegraded(st, &Ping{LossDegradedPct: 20}) {
		t.Fatal("loss 50 > 20 ⇒ degraded")
	}
	if !pingDegraded(st, &Ping{RttDegradedMs: 80}) {
		t.Fatal("avg 100 > 80 ⇒ degraded")
	}
	if pingDegraded(st, &Ping{LossDegradedPct: 60, RttDegradedMs: 120}) {
		t.Fatal("under both thresholds ⇒ not degraded")
	}
	if pingDegraded(PingStats{Received: 0, LossPct: 100}, &Ping{LossDegradedPct: 20}) {
		t.Fatal("total loss is DOWN, not degraded")
	}
}
