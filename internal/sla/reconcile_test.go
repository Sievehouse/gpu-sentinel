package sla

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Sievehouse/gpu-sentinel/internal/contract"
	"github.com/Sievehouse/gpu-sentinel/internal/events"
	"github.com/Sievehouse/gpu-sentinel/internal/interval"
)

func set(ivs ...[2]int64) interval.Set {
	var out []interval.Interval
	for _, iv := range ivs {
		out = append(out, interval.Interval{Start: iv[0], End: iv[1]})
	}
	return interval.Normalize(out)
}

func TestReconcileSides(t *testing.T) {
	rule := func(count, start, end string) *contract.Reconcile {
		return &contract.Reconcile{Count: count, Start: start, End: end}
	}
	cases := []struct {
		name      string
		r         *contract.Reconcile
		prov, ten interval.Set
		want      interval.Set
	}{
		{"union", rule("either", "earliest", "latest"), set([2]int64{10, 20}), set([2]int64{12, 25}), set([2]int64{10, 25})},
		{"overlap only", rule("both", "latest", "earliest"), set([2]int64{10, 20}), set([2]int64{12, 25}), set([2]int64{12, 20})},
		{"fault to job-visible", rule("either", "provider", "tenant"), set([2]int64{10, 20}), set([2]int64{12, 25}), set([2]int64{10, 25})},
		{"tenant start, provider end", rule("either", "tenant", "provider"), set([2]int64{10, 20}), set([2]int64{12, 25}), set([2]int64{12, 20})},
		{"both needs both", rule("both", "earliest", "latest"), set([2]int64{10, 20}), nil, nil},
		{"tenant needs tenant", rule("tenant", "earliest", "latest"), set([2]int64{10, 20}), nil, nil},
		{"provider needs provider", rule("provider", "earliest", "latest"), nil, set([2]int64{30, 40}), nil},
		{"missing clock falls back", rule("either", "tenant", "tenant"), set([2]int64{10, 20}), nil, set([2]int64{10, 20})},
		{"episode spans a gap", rule("either", "provider", "provider"), set([2]int64{10, 20}, [2]int64{30, 40}), set([2]int64{15, 35}), set([2]int64{10, 40})},
		{"inner clocks across a gap", rule("both", "latest", "earliest"), set([2]int64{10, 20}, [2]int64{30, 40}), set([2]int64{15, 35}), set([2]int64{15, 35})},
		{"separate episodes", rule("either", "earliest", "latest"), set([2]int64{0, 5}), set([2]int64{10, 15}), set([2]int64{0, 5}, [2]int64{10, 15})},
		{"end before start is dropped", rule("either", "tenant", "provider"), set([2]int64{10, 12}), set([2]int64{12, 20}), nil},
	}
	for _, c := range cases {
		got := reconcileSides(c.r, c.prov, c.ten)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

const reconciledContract = "../../examples/contracts/hgx-16-node-reconciled.yaml"
const bothEvents = "../../examples/events/hgx-16-node-2026-10-both.jsonl"

// Every number is worked out by hand in docs/sla-spec.md.
func TestReconciledExample(t *testing.T) {
	spec, evs := load(t, reconciledContract, bothEvents)
	st, err := Compute(spec, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]int64{
		"gpu-003": {8940, 0},  // provider's 04:12 to the tenant's 06:41
		"gpu-005": {3720, 0},  // period start to the tenant's 01:02
		"gpu-009": {3000, 0},  // only the tenant saw it; counts under "either"
		"gpu-010": {0, 10800}, // tenant's 30 s after announced maintenance is under 5 min
		"gpu-011": {7200, 0},  // unannounced maintenance, seen by both
		"gpu-012": {0, 5400},  // customer-caused in the provider's record: excluded
		"gpu-014": {14400, 0}, // degraded on both sides
		"gpu-016": {93600, 0}, // provider's 22:00 to period end
	}
	if got := nodeSeconds(st); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if st.Summary.DowntimeNodeSeconds != 130860 || st.Summary.AvailabilityPercent != "99.6946" || st.Credit.Percent != 5 {
		t.Fatalf("summary %+v credit %+v", st.Summary, st.Credit)
	}
	if st.EngineVersion != Semantics030 {
		t.Fatalf("engine version %s", st.EngineVersion)
	}
	// Pinned in docs/sla-spec.md. If this changes, the 0.3 semantics changed.
	if st.StatementSHA256 != "793920cf0be5f2c1e7ce83614657a28ae27daf291a68e56bbd4857e470879f73" {
		t.Fatalf("statement hash %s", st.StatementSHA256)
	}
	seen := map[string]string{}
	for _, n := range st.Nodes {
		for _, in := range n.Incidents {
			seen[n.Node] = in.SeenBy
		}
	}
	if seen["gpu-009"] != "tenant" || seen["gpu-003"] != "both" || seen["gpu-016"] != "both" {
		t.Fatalf("seenBy %v", seen)
	}
	if err := Replay(spec, evs, st); err != nil {
		t.Fatal(err)
	}
}

func withRule(t *testing.T, count, start, end string) *contract.Spec {
	t.Helper()
	spec, err := contract.Load(reconciledContract)
	if err != nil {
		t.Fatal(err)
	}
	c := spec.Contract
	r := *c.Measurement.Reconcile
	r.Count, r.Start, r.End = count, start, end
	c.Measurement.Reconcile = &r
	out, err := contract.Parse(c)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReconcileRulesOnTheExample(t *testing.T) {
	_, evs := load(t, reconciledContract, bothEvents)
	cases := []struct {
		count, start, end string
		total             int64
	}{
		// The overlap of the two records: the same 127,580 node-seconds that
		// corroboration reports as "seen by both".
		{"both", "latest", "earliest", 127580},
		// Customer-friendly: anything either party saw, as long as anyone saw it.
		{"either", "earliest", "latest", 130860},
		// The provider's record alone, as in version 1 of the contract.
		{"provider", "provider", "provider", 127680},
		// The tenant's clock alone. Exclusions still apply whichever party
		// recorded them, so the provider's "customer-caused" on gpu-012 takes
		// 5,400 s off the tenant's own 136,160.
		{"tenant", "tenant", "tenant", 130760},
	}
	for _, c := range cases {
		st, err := Compute(withRule(t, c.count, c.start, c.end), evs, october)
		if err != nil {
			t.Fatal(err)
		}
		if st.Summary.DowntimeNodeSeconds != c.total {
			t.Errorf("%s/%s/%s: %d node-seconds, want %d", c.count, c.start, c.end, st.Summary.DowntimeNodeSeconds, c.total)
		}
	}
}

func ev(id, ts, node string, typ events.Type, source, role string) events.Event {
	e := events.Event{ID: id, TS: ts, Node: node, Type: typ, Source: source, SignerRole: role}
	if err := e.Normalize(); err != nil {
		panic(err)
	}
	return e
}

func TestUnlistedSources(t *testing.T) {
	spec, _ := load(t, reconciledContract, bothEvents)
	evs := []events.Event{
		ev("a", "2026-10-02T00:00:00Z", "gpu-001", events.Down, "manual", ""),
		ev("b", "2026-10-02T01:00:00Z", "gpu-001", events.Up, "manual", ""),
	}
	if _, err := Compute(spec, evs, october); err == nil || !strings.Contains(err.Error(), "source manual is not listed") {
		t.Fatalf("unlisted source not rejected: %v", err)
	}
	c := spec.Contract
	src := *c.Sources
	src.Unlisted = contract.UnlistedIgnore
	c.Sources = &src
	lenient, err := contract.Parse(c)
	if err != nil {
		t.Fatal(err)
	}
	st, err := Compute(lenient, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	if st.Summary.DowntimeNodeSeconds != 0 || len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "2 event(s) from sources the contract does not list were ignored: source manual") {
		t.Fatalf("warnings %v", st.Warnings)
	}
}

func TestSignerRoleDecidesTheSide(t *testing.T) {
	spec, _ := load(t, reconciledContract, bothEvents)
	// A provider key cannot pass its events off as the tenant's by naming
	// the tenant's source: with a signer role, only the role counts.
	if side, ok := spec.SideOf("provider-agent", "tenant-probe"); !ok || side != contract.SideProvider {
		t.Fatalf("got %s %v", side, ok)
	}
	if side, ok := spec.SideOf("tenant-probe", "capledger-agent"); !ok || side != contract.SideTenant {
		t.Fatalf("got %s %v", side, ok)
	}
	if _, ok := spec.SideOf("auditor", "capledger-agent"); ok {
		t.Fatal("an unlisted role must not fall back to the source name")
	}
	// Only the tenant recorded this outage, and the contract counts only
	// what both parties saw: nothing counts.
	evs := []events.Event{
		ev("a", "2026-10-02T00:00:00Z", "gpu-001", events.Down, "x", "tenant-probe"),
		ev("b", "2026-10-02T01:00:00Z", "gpu-001", events.Up, "x", "tenant-probe"),
	}
	st, err := Compute(withRule(t, "both", "earliest", "latest"), evs, october)
	if err != nil {
		t.Fatal(err)
	}
	if st.Summary.DowntimeNodeSeconds != 0 {
		t.Fatalf("downtime %d", st.Summary.DowntimeNodeSeconds)
	}
}

func TestCorroborateUnderReconciledContract(t *testing.T) {
	spec, evs := load(t, reconciledContract, bothEvents)
	prov, ten, err := SplitBySide(spec, evs)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Corroborate(spec, prov, ten, "provider", "tenant", october)
	if err != nil {
		t.Fatal(err)
	}
	// The same comparison as with the two separate example files.
	if c.Summary.BothNodeSeconds != 127580 || c.Summary.OnlyANodeSeconds != 100 || c.Summary.OnlyBNodeSeconds != 8580 || c.Summary.AgreementPercent != "93.63" {
		t.Fatalf("summary %+v", c.Summary)
	}
	if _, _, err := SplitBySide(withoutTenant(t), evs); err == nil {
		t.Fatal("split without sources.tenant must fail")
	}
}

func withoutTenant(t *testing.T) *contract.Spec {
	t.Helper()
	spec, err := contract.Load("../../examples/contracts/hgx-16-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return spec
}
