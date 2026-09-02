package main

import "testing"

func wi(id string, interval int) WorkItem {
	return WorkItem{MonitorID: id, IntervalSeconds: interval}
}

func ids(items []WorkItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.MonitorID)
	}
	return out
}

func TestDueItems(t *testing.T) {
	work := []WorkItem{wi("a", 60), wi("b", 300), wi("c", 0)}
	now := int64(10000)

	// never probed -> all due
	if got := ids(dueItems(work, map[string]int64{}, now)); len(got) != 3 {
		t.Fatalf("never-probed items should all be due, got %v", got)
	}

	// a last probed 70s ago (>=60 due); b 100s ago (<300 not due); c interval 0 always due
	last := map[string]int64{"a": now - 70, "b": now - 100, "c": now - 5}
	got := ids(dueItems(work, last, now))
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("expected [a c], got %v", got)
	}

	// boundary: exactly the interval elapsed -> due (>=)
	if len(dueItems([]WorkItem{wi("a", 60)}, map[string]int64{"a": now - 60}, now)) != 1 {
		t.Fatal("exactly intervalSeconds elapsed should be due")
	}
	// boundary: one second short -> not due
	if len(dueItems([]WorkItem{wi("a", 60)}, map[string]int64{"a": now - 59}, now)) != 0 {
		t.Fatal("under intervalSeconds should not be due")
	}
}
