package sla

import (
	"strings"
	"testing"
)

// Worked example in docs/tenant-probe.md.
func TestCorroborateProviderAndTenant(t *testing.T) {
	spec, provider := load(t, "../../examples/contracts/hgx-16-node.json", "../../examples/events/hgx-16-node-2026-10.jsonl")
	_, tenant := load(t, "../../examples/contracts/hgx-16-node.json", "../../examples/events/hgx-16-node-2026-10-tenant.jsonl")
	c, err := Corroborate(spec, provider, tenant, "provider", "tenant", october)
	if err != nil {
		t.Fatal(err)
	}
	if c.A.DowntimeNodeSeconds != 127680 || c.B.DowntimeNodeSeconds != 136160 || c.B.AvailabilityPercent != "99.6823" {
		t.Fatalf("sides %+v %+v", c.A, c.B)
	}
	s := c.Summary
	if s.BothNodeSeconds != 127580 || s.OnlyANodeSeconds != 100 || s.OnlyBNodeSeconds != 8580 || s.AgreementPercent != "93.63" || s.NodesInDispute != 5 {
		t.Fatalf("summary %+v", s)
	}
	byNode := map[string]CorroboratedNode{}
	for _, n := range c.Nodes {
		byNode[n.Node] = n
	}
	if n := byNode["gpu-009"]; n.OnlyBSeconds != 3000 || len(n.OnlyB) != 1 || n.OnlyB[0].Start != "2026-10-18T07:00:00Z" {
		t.Fatalf("gpu-009 (seen only by the tenant) %+v", n)
	}
	if n := byNode["gpu-003"]; n.BothSeconds != 8820 || n.OnlyASeconds != 60 || n.OnlyBSeconds != 60 {
		t.Fatalf("gpu-003 %+v", n)
	}
	if _, ok := byNode["gpu-010"]; ok {
		t.Fatal("gpu-010 was in announced maintenance on both sides and must not appear")
	}
	if c.ReportSHA256 == "" || !strings.Contains(c.Text(), "93.63%") {
		t.Fatal("report not sealed or text missing agreement")
	}
	if _, err := Corroborate(spec, provider, tenant, "x", "x", october); err == nil {
		t.Fatal("identical labels accepted")
	}
}
