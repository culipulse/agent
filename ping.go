package main

import (
	"math"
	"time"
)

// PingStats summarizes one multi-packet ICMP check. Display-only (Detail.ping); never feeds consensus.
type PingStats struct {
	Sent, Received                            int
	LossPct, RttMinMs, RttAvgMs, RttMaxMs, RttStddevMs float64
}

// computePingStats reduces the received round-trip times (out of `sent` echoes) to loss% + RTT stats.
// LossPct is 0..100; RTT stats are over RECEIVED packets only (0 when none). Stddev is population stddev.
func computePingStats(sent int, rtts []time.Duration) PingStats {
	st := PingStats{Sent: sent, Received: len(rtts)}
	if sent > 0 {
		st.LossPct = float64(sent-len(rtts)) / float64(sent) * 100
	}
	if len(rtts) == 0 {
		return st
	}
	vals := make([]float64, len(rtts))
	min, max, sum := math.MaxFloat64, 0.0, 0.0
	for i, d := range rtts {
		v := float64(d.Microseconds()) / 1000.0
		vals[i] = v
		sum += v
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	avg := sum / float64(len(vals))
	var sq float64
	for _, v := range vals {
		sq += (v - avg) * (v - avg)
	}
	st.RttMinMs, st.RttMaxMs, st.RttAvgMs = min, max, avg
	st.RttStddevMs = math.Sqrt(sq / float64(len(vals)))
	return st
}

// pingCount returns the echoes-per-check, defaulting to 4 and clamping to 1..10.
func pingCount(p *Ping) int {
	n := 4
	if p != nil && p.Count != 0 {
		n = p.Count
	}
	if n < 1 {
		n = 1
	}
	if n > 10 {
		n = 10
	}
	return n
}

// pingDegraded reports whether the check is "up but degraded" per the optional thresholds. A check with
// zero received packets is DOWN (handled by the caller), never degraded. Absent config ⇒ never degraded.
func pingDegraded(st PingStats, p *Ping) bool {
	if p == nil || st.Received == 0 {
		return false
	}
	if p.LossDegradedPct > 0 && st.LossPct > float64(p.LossDegradedPct) {
		return true
	}
	if p.RttDegradedMs > 0 && st.RttAvgMs > float64(p.RttDegradedMs) {
		return true
	}
	return false
}
