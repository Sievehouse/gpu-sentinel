package sla

import (
	"fmt"
	"math/big"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/Sievehouse/capacity-ledger/internal/contract"
	"github.com/Sievehouse/capacity-ledger/internal/events"
)

const oct2026Start, oct2026End = 1790812800, 1793491200 // 2026-10-01T00:00:00Z, 2026-11-01T00:00:00Z

var october = Period{Start: oct2026Start, End: oct2026End}

func load(t *testing.T, contractPath, eventsPath string) (*contract.Spec, []events.Event) {
	t.Helper()
	spec, err := contract.Load(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	evs, err := events.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	return spec, evs
}

func nodeSeconds(st *Statement) map[string][2]int64 {
	out := map[string][2]int64{}
	for _, n := range st.Nodes {
		out[n.Node] = [2]int64{n.DowntimeSeconds, n.ExcludedSeconds}
	}
	return out
}

func TestPeriodConstants(t *testing.T) {
	s, _ := events.ParseTime("2026-10-01T00:00:00Z")
	e, _ := events.ParseTime("2026-11-01T00:00:00Z")
	if s != oct2026Start || e != oct2026End {
		t.Fatalf("period constants wrong: %d %d", s, e)
	}
}

// Every expected number below is worked out by hand in docs/sla-spec.md.
func TestHGXExample(t *testing.T) {
	spec, evs := load(t, "../../examples/contracts/hgx-16-node.json", "../../examples/events/hgx-16-node-2026-10.jsonl")
	st, err := Compute(spec, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int64{
		"gpu-003": {8880, 0},  // 04:12-06:40
		"gpu-005": {3600, 0},  // down since Sep 30, counted from Oct 1 00:00
		"gpu-010": {0, 10800}, // 3 h maintenance announced 98 h ahead: excluded
		"gpu-011": {7200, 0},  // 2 h maintenance announced 5 h ahead: counts
		"gpu-012": {0, 5400},  // customer-caused: excluded
		"gpu-014": {14400, 0}, // 4 h degraded counts as down
		"gpu-016": {93600, 0}, // down at Oct 30 22:00, still down at period end
	}
	got := nodeSeconds(st)
	if len(got) != len(want) {
		t.Fatalf("nodes with downtime or exclusions: got %v", got)
	}
	for n, w := range want {
		if got[n] != w {
			t.Errorf("%s: got downtime/excluded %v, want %v", n, got[n], w)
		}
	}
	// gpu-007's 3-minute blip is below the 5-minute minimum.
	if _, ok := got["gpu-007"]; ok {
		t.Error("gpu-007's 3-minute blip should not count")
	}
	s := st.Summary
	if s.DowntimeNodeSeconds != 127680 || s.ExcludedNodeSeconds != 16200 || s.NodeSecondsTotal != 16*2678400 {
		t.Fatalf("summary totals wrong: %+v", s)
	}
	if s.AvailabilityPercent != "99.7021" || s.DowntimeGPUHours != "283.73" {
		t.Fatalf("availability %s, GPU-hours %s", s.AvailabilityPercent, s.DowntimeGPUHours)
	}
	if st.Credit.Percent != 5 || st.Credit.TierAvailabilityBelow != "99.9" || st.Credit.Amount != "15000.00" {
		t.Fatalf("credit wrong: %+v", st.Credit)
	}
	if len(st.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", st.Warnings)
	}
}

func TestNVL72RackRule(t *testing.T) {
	spec, evs := load(t, "../../examples/contracts/nvl72-2-racks.json", "../../examples/events/nvl72-2026-10.jsonl")
	st, err := Compute(spec, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	got := nodeSeconds(st)
	if got["rack-a-tray-03"][0] != 7200 || got["rack-a-tray-07"][0] != 4500 || got["rack-a-tray-11"][0] != 3600 {
		t.Fatalf("faulty trays wrong: %v", got)
	}
	// The 15 healthy trays of rack-a are down for the 45 minutes when three trays overlapped.
	for i := 1; i <= 18; i++ {
		n := fmt.Sprintf("rack-a-tray-%02d", i)
		if i == 3 || i == 7 || i == 11 {
			continue
		}
		if got[n][0] != 2700 {
			t.Fatalf("%s: got %d s, want 2700 s from the rack rule", n, got[n][0])
		}
	}
	if got["rack-b-tray-02"][0] != 1200 {
		t.Fatalf("rack-b tray-02: %v", got["rack-b-tray-02"])
	}
	if st.Summary.DowntimeNodeSeconds != 57000 || st.Summary.AvailabilityPercent != "99.9409" {
		t.Fatalf("summary wrong: %+v", st.Summary)
	}
	if st.Credit.Percent != 3 || st.Credit.Amount != "19500.00" {
		t.Fatalf("credit wrong: %+v", st.Credit)
	}
	for _, n := range st.Nodes {
		if n.Node != "rack-a-tray-01" {
			continue
		}
		if len(n.Incidents) != 1 || n.Incidents[0].Via != "rack:rack-a" || strings.Join(n.Incidents[0].EventIDs, ",") != "r-0001,r-0002,r-0003,r-0004,r-0005,r-0006" {
			t.Fatalf("rack-rule incident not attributed to its triggers: %+v", n.Incidents)
		}
	}
}

func TestStatementIsDeterministicAndReplayable(t *testing.T) {
	spec, evs := load(t, "../../examples/contracts/hgx-16-node.json", "../../examples/events/hgx-16-node-2026-10.jsonl")
	first, err := Compute(spec, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		shuffled := append([]events.Event(nil), evs...)
		r.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		again, err := Compute(spec, shuffled, october)
		if err != nil {
			t.Fatal(err)
		}
		if again.StatementSHA256 != first.StatementSHA256 {
			t.Fatal("statement hash depends on event input order")
		}
	}
	if err := Replay(spec, evs, first); err != nil {
		t.Fatalf("replay of an untouched statement failed: %v", err)
	}
	tampered := *first
	tampered.Summary.AvailabilityPercent = "99.9500"
	if err := tampered.CheckSeal(); err == nil {
		t.Fatal("edited statement passed its seal check")
	}
	if err := Replay(spec, evs[1:], first); err == nil {
		t.Fatal("replay with a missing event should fail")
	}
}

func availability(st *Statement) *big.Rat {
	r, _ := new(big.Rat).SetString(st.Summary.AvailabilityPercent)
	return r
}

// randomEvents builds a random event stream on the given nodes.
func randomEvents(r *rand.Rand, trial int, nodes []string, customerCauses bool) []events.Event {
	types := []events.Type{events.Down, events.Up, events.DegradedStart, events.DegradedEnd, events.MaintenanceStart, events.MaintenanceEnd}
	var evs []events.Event
	for i := 0; i < 30; i++ {
		e := events.Event{
			ID:   fmt.Sprintf("t%d-%d", trial, i),
			TS:   events.FormatTime(oct2026Start + r.Int63n(oct2026End-oct2026Start)),
			Node: nodes[r.Intn(len(nodes))],
			Type: types[r.Intn(len(types))],
		}
		if customerCauses && r.Intn(3) == 0 {
			e.Cause = "customer:test"
		}
		if e.Type == events.MaintenanceStart && r.Intn(2) == 0 {
			e.NoticeTS = events.FormatTime(oct2026Start - 7*24*3600)
		}
		evs = append(evs, e)
	}
	return evs
}

func mustCompute(t *testing.T, spec *contract.Spec, evs []events.Event) *Statement {
	t.Helper()
	if err := events.NormalizeAll(evs); err != nil {
		t.Fatal(err)
	}
	st, err := Compute(spec, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// An outage on a node with no other events must lower availability and can
// never lower the credit, whatever happens on the other nodes.
func TestMonotonicityIndependentOutage(t *testing.T) {
	spec, _ := load(t, "../../examples/contracts/hgx-16-node.json", "../../examples/events/hgx-16-node-2026-10.jsonl")
	r := rand.New(rand.NewSource(7))
	nodes := spec.Contract.Cluster.Nodes
	for trial := 0; trial < 300; trial++ {
		evs := randomEvents(r, trial, nodes[:12], true)
		before := mustCompute(t, spec, evs)
		start := oct2026Start + r.Int63n(oct2026End-oct2026Start-7200)
		node := nodes[12+r.Intn(4)]
		extra := []events.Event{
			{ID: "extra-down", TS: events.FormatTime(start), Node: node, Type: events.Down, Cause: "xid:79"},
			{ID: "extra-up", TS: events.FormatTime(start + 3600), Node: node, Type: events.Up},
		}
		after := mustCompute(t, spec, append(append([]events.Event(nil), evs...), extra...))
		if availability(after).Cmp(availability(before)) >= 0 {
			t.Fatalf("trial %d: availability did not fall (%s -> %s)", trial, before.Summary.AvailabilityPercent, after.Summary.AvailabilityPercent)
		}
		if after.Credit.Percent < before.Credit.Percent {
			t.Fatalf("trial %d: credit fell after adding an outage", trial)
		}
	}
}

// One more down event can only open or extend an outage, so availability can
// never rise. (A down/up pair is different: its up event legitimately ends
// an outage that was already open, which can shorten it.)
func TestMonotonicityExtraDownEvent(t *testing.T) {
	spec, _ := load(t, "../../examples/contracts/hgx-16-node.json", "../../examples/events/hgx-16-node-2026-10.jsonl")
	r := rand.New(rand.NewSource(11))
	nodes := spec.Contract.Cluster.Nodes
	for trial := 0; trial < 300; trial++ {
		evs := randomEvents(r, trial, nodes, false)
		before := mustCompute(t, spec, evs)
		extra := events.Event{
			ID:   "extra-down",
			TS:   events.FormatTime(oct2026Start + r.Int63n(oct2026End-oct2026Start)),
			Node: nodes[r.Intn(len(nodes))], Type: events.Down, Cause: "xid:79",
		}
		after := mustCompute(t, spec, append(append([]events.Event(nil), evs...), extra))
		if availability(after).Cmp(availability(before)) > 0 {
			t.Fatalf("trial %d: availability rose from %s to %s after an extra down event on %s at %s",
				trial, before.Summary.AvailabilityPercent, after.Summary.AvailabilityPercent, extra.Node, extra.TS)
		}
	}
}

func TestExclusionCapAndMinIncident(t *testing.T) {
	c := contract.Contract{
		APIVersion: contract.APIVersion, Kind: contract.Kind,
		Metadata:    contract.Metadata{ID: "cap", Version: 1},
		Cluster:     contract.Cluster{GPUsPerNode: 8, Nodes: []string{"n1", "n2"}},
		Measurement: contract.Measurement{MinIncident: "5m"},
		Exclusions:  contract.Exclusions{MaintenanceNotice: "72h", MaxExcludedPerNode: "8h"},
		Credits:     contract.Credits{Tiers: []contract.Tier{{AvailabilityBelow: "99.9", CreditPercent: 5}}},
	}
	spec, err := contract.Parse(c)
	if err != nil {
		t.Fatal(err)
	}
	evs := []events.Event{
		// 10 h of well-announced maintenance on n1: 8 h excluded, 2 h counts.
		{ID: "m1", TS: "2026-10-10T00:00:00Z", Node: "n1", Type: events.MaintenanceStart, NoticeTS: "2026-10-01T00:00:00Z"},
		{ID: "m2", TS: "2026-10-10T10:00:00Z", Node: "n1", Type: events.MaintenanceEnd},
		// 4 m 59 s outage on n2 is ignored, exactly 5 m counts.
		{ID: "d1", TS: "2026-10-11T00:00:00Z", Node: "n2", Type: events.Down},
		{ID: "u1", TS: "2026-10-11T00:04:59Z", Node: "n2", Type: events.Up},
		{ID: "d2", TS: "2026-10-12T00:00:00Z", Node: "n2", Type: events.Down},
		{ID: "u2", TS: "2026-10-12T00:05:00Z", Node: "n2", Type: events.Up},
		// Unknown node: ignored with a warning.
		{ID: "x1", TS: "2026-10-12T00:00:00Z", Node: "n9", Type: events.Down},
	}
	if err := events.NormalizeAll(evs); err != nil {
		t.Fatal(err)
	}
	st, err := Compute(spec, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	got := nodeSeconds(st)
	if got["n1"] != [2]int64{7200, 28800} {
		t.Fatalf("n1: got %v, want 2 h counted and 8 h excluded", got["n1"])
	}
	if got["n2"] != [2]int64{300, 0} {
		t.Fatalf("n2: got %v, want only the 5-minute outage", got["n2"])
	}
	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "n9") {
		t.Fatalf("expected one unknown-node warning, got %v", st.Warnings)
	}
}

// Statements published with v0.1 must keep their exact hashes: the 0.2
// engine stamps semantics 0.1.0 when no 0.2 feature is used.
func TestGoldenHashesFromV01(t *testing.T) {
	cases := []struct{ contract, events, hash string }{
		{"../../examples/contracts/hgx-16-node.json", "../../examples/events/hgx-16-node-2026-10.jsonl", "d6280aea2086f9bf153922421dffbc4d44fbe17cc1106ba5184cad29f99323fc"},
		{"../../examples/contracts/hgx-16-node.yaml", "../../examples/events/hgx-16-node-2026-10.jsonl", "d6280aea2086f9bf153922421dffbc4d44fbe17cc1106ba5184cad29f99323fc"},
		{"../../examples/contracts/nvl72-2-racks.json", "../../examples/events/nvl72-2026-10.jsonl", "9099227ae4737d87a2b556667047a3e0f06ac521c04e4e8c3a73a4ac95e9578b"},
	}
	for _, c := range cases {
		spec, evs := load(t, c.contract, c.events)
		st, err := Compute(spec, evs, october)
		if err != nil {
			t.Fatal(err)
		}
		if st.StatementSHA256 != c.hash || st.EngineVersion != Semantics010 {
			t.Errorf("%s: hash %s (semantics %s), want %s (0.1.0)", c.contract, st.StatementSHA256, st.EngineVersion, c.hash)
		}
	}
}

func loadAll(t *testing.T, contractPath string, eventPaths ...string) (*contract.Spec, []events.Event) {
	t.Helper()
	spec, err := contract.Load(contractPath)
	if err != nil {
		t.Fatal(err)
	}
	var all []events.Event
	for _, p := range eventPaths {
		evs, err := events.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, evs...)
	}
	return spec, all
}

func unmonitoredByNode(st *Statement) map[string]int64 {
	out := map[string]int64{}
	for _, n := range st.Nodes {
		if n.UnmonitoredSeconds > 0 {
			out[n.Node] = n.UnmonitoredSeconds
		}
	}
	return out
}

// Worked example in docs/sla-spec.md ("Unmonitored time").
func TestUnmonitoredReportOnly(t *testing.T) {
	spec, evs := loadAll(t, "../../examples/contracts/hgx-16-node.yaml",
		"../../examples/events/hgx-16-node-2026-10.jsonl", "../../examples/events/hgx-16-node-2026-10-gaps.jsonl")
	st, err := Compute(spec, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	if st.Summary.DowntimeNodeSeconds != 127680 || st.Summary.AvailabilityPercent != "99.7021" || st.Credit.Percent != 5 {
		t.Fatalf("report-only gaps must not change downtime: %+v %+v", st.Summary, st.Credit)
	}
	want := map[string]int64{"gpu-008": 172800, "gpu-010": 9000, "gpu-016": 93300}
	if got := unmonitoredByNode(st); !reflect.DeepEqual(got, want) {
		t.Fatalf("unmonitored %v, want %v", got, want)
	}
	if st.Summary.UnmonitoredNodeSeconds != 275100 || st.EngineVersion != Semantics020 {
		t.Fatalf("summary %+v, semantics %s", st.Summary, st.EngineVersion)
	}
	if err := Replay(spec, evs, st); err != nil {
		t.Fatal(err)
	}
}

func TestUnmonitoredCountAsDown(t *testing.T) {
	spec, evs := loadAll(t, "../../examples/contracts/hgx-16-node-strict.yaml",
		"../../examples/events/hgx-16-node-2026-10.jsonl", "../../examples/events/hgx-16-node-2026-10-gaps.jsonl")
	st, err := Compute(spec, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	got := nodeSeconds(st)
	if got["gpu-008"] != [2]int64{172800, 0} {
		t.Errorf("gpu-008: 48 h of silence should count: %v", got["gpu-008"])
	}
	if got["gpu-010"] != [2]int64{0, 10800} {
		t.Errorf("gpu-010: silence inside announced maintenance stays excluded: %v", got["gpu-010"])
	}
	if got["gpu-016"] != [2]int64{93600, 0} {
		t.Errorf("gpu-016: silence during an outage must not be counted twice: %v", got["gpu-016"])
	}
	if st.Summary.DowntimeNodeSeconds != 300480 || st.Summary.AvailabilityPercent != "99.2988" {
		t.Fatalf("summary %+v", st.Summary)
	}
	if st.Credit.Percent != 10 || st.Credit.Amount != "30000.00" || st.Summary.UnmonitoredPolicy != "count-as-down" {
		t.Fatalf("credit %+v", st.Credit)
	}
	for _, n := range st.Nodes {
		if n.Node == "gpu-008" && (len(n.Incidents) != 1 || strings.Join(n.Incidents[0].Causes, ",") != "no-heartbeat") {
			t.Fatalf("gpu-008 incident not attributed to the missing heartbeat: %+v", n.Incidents)
		}
	}
}

func TestReplayRefusesUnknownSemantics(t *testing.T) {
	spec, evs := load(t, "../../examples/contracts/hgx-16-node.json", "../../examples/events/hgx-16-node-2026-10.jsonl")
	st, err := Compute(spec, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	st.EngineVersion = "9.9.9"
	if err := st.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := Replay(spec, evs, st); err == nil || !strings.Contains(err.Error(), "needs engine semantics 9.9.9") {
		t.Fatalf("got %v", err)
	}
}
