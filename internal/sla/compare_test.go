package sla

import (
	"os"
	"strings"
	"testing"
)

func TestReadProviderReport(t *testing.T) {
	r, err := ReadProviderReport(strings.NewReader("# comment\nnode,downtime_seconds\n\ngpu-003, 7200\ngpu-016,28800\n"))
	if err != nil || len(r) != 2 || r["gpu-003"] != 7200 {
		t.Fatalf("%v %v", r, err)
	}
	for _, bad := range []string{"gpu-003,x\ngpu-004,y\n", "gpu-003,1\ngpu-003,2\n", "gpu-003,-5\n", "gpu-003\n", "gpu-003,1\nnode,seconds\n"} {
		if _, err := ReadProviderReport(strings.NewReader(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestCompare(t *testing.T) {
	spec, evs := loadAll(t, "../../examples/contracts/hgx-16-node.yaml", "../../examples/events/hgx-16-node-2026-10.jsonl")
	st, err := Compute(spec, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open("../../examples/pilot/provider-report-2026-10.csv")
	defer f.Close()
	rep, err := ReadProviderReport(f)
	if err != nil {
		t.Fatal(err)
	}
	rep["gpu-999"] = 60
	c, err := Compare(st, rep, 300, spec)
	if err != nil {
		t.Fatal(err)
	}
	if c.ProviderDowntime != 36000 || c.ProviderAvailability != "99.9160" || c.ProviderCredit.Percent != 0 || c.StatementCredit.Percent != 5 {
		t.Fatalf("totals: %+v %+v", c, c.ProviderCredit)
	}
	if c.Flagged != 5 || len(c.Nodes) != 5 || c.Nodes[0].Node != "gpu-003" || c.Nodes[0].Difference != -1680 {
		t.Fatalf("nodes: %+v", c.Nodes)
	}
	if len(c.UnknownNodes) != 1 || c.UnknownNodes[0] != "gpu-999" {
		t.Fatalf("unknown: %v", c.UnknownNodes)
	}
	// A generous tolerance flags only the big gaps.
	c, _ = Compare(st, rep, 3600, spec)
	if c.Flagged != 3 {
		t.Fatalf("tolerance 3600: %d flagged", c.Flagged)
	}
	// Without the contract: no credit for the provider's figures, every node counts.
	c, _ = Compare(st, rep, 300, nil)
	if c.ProviderCredit != nil || c.ProviderDowntime != 36060 {
		t.Fatalf("without contract: %+v", c)
	}
	if !strings.Contains(c.Text(), "give --contract") {
		t.Fatal(c.Text())
	}
	// A tampered statement is refused.
	st.Summary.AvailabilityPercent = "100.0000"
	if _, err := Compare(st, rep, 300, spec); err == nil {
		t.Fatal("tampered statement accepted")
	}
}
