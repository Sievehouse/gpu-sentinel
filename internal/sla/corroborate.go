package sla

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/Sievehouse/gpu-sentinel/internal/canonical"
	"github.com/Sievehouse/gpu-sentinel/internal/contract"
	"github.com/Sievehouse/gpu-sentinel/internal/events"
	"github.com/Sievehouse/gpu-sentinel/internal/interval"
)

// CorroborationKind identifies a corroboration report.
const CorroborationKind = "CorroborationReport"

// Corroboration compares the countable downtime that two independent
// sources (usually the provider's agent and the tenant's probe) recorded for
// the same contract and period. Agreement builds trust in both; each
// disagreement is a concrete stretch of time to investigate.
type Corroboration struct {
	APIVersion   string             `json:"apiVersion"`
	Kind         string             `json:"kind"`
	Contract     ContractRef        `json:"contract"`
	Period       PeriodRef          `json:"period"`
	A            Side               `json:"a"`
	B            Side               `json:"b"`
	Summary      CorroborationSum   `json:"summary"`
	Nodes        []CorroboratedNode `json:"nodes"`
	ReportSHA256 string             `json:"reportSha256"`
}

// Side summarises one source's statement.
type Side struct {
	Label               string `json:"label"`
	StatementSHA256     string `json:"statementSha256"`
	EventsSHA256        string `json:"eventsSha256"`
	AvailabilityPercent string `json:"availabilityPercent"`
	DowntimeNodeSeconds int64  `json:"downtimeNodeSeconds"`
	CreditPercent       int    `json:"creditPercent"`
}

// CorroborationSum holds cluster totals.
type CorroborationSum struct {
	BothNodeSeconds  int64  `json:"bothNodeSeconds"`
	OnlyANodeSeconds int64  `json:"onlyANodeSeconds"`
	OnlyBNodeSeconds int64  `json:"onlyBNodeSeconds"`
	AgreementPercent string `json:"agreementPercent"`
	NodesInDispute   int    `json:"nodesInDispute"`
}

// CorroboratedNode compares one node. Nodes where neither side recorded
// downtime are omitted.
type CorroboratedNode struct {
	Node         string `json:"node"`
	ASeconds     int64  `json:"aSeconds"`
	BSeconds     int64  `json:"bSeconds"`
	BothSeconds  int64  `json:"bothSeconds"`
	OnlyASeconds int64  `json:"onlyASeconds"`
	OnlyBSeconds int64  `json:"onlyBSeconds"`
	OnlyA        []Span `json:"onlyA,omitempty"`
	OnlyB        []Span `json:"onlyB,omitempty"`
}

// Corroborate computes a statement from each event set and compares them.
func Corroborate(spec *contract.Spec, evsA, evsB []events.Event, labelA, labelB string, p Period) (*Corroboration, error) {
	if labelA == "" || labelB == "" || labelA == labelB {
		return nil, fmt.Errorf("the two sides need different, non-empty labels")
	}
	// Each side is computed on its own, without reconciliation: the point is
	// to compare what each party recorded, not the combined clock.
	stA, err := compute(spec, evsA, p, false)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", labelA, err)
	}
	stB, err := compute(spec, evsB, p, false)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", labelB, err)
	}
	setsA, err := stA.DowntimeSets()
	if err != nil {
		return nil, err
	}
	setsB, err := stB.DowntimeSets()
	if err != nil {
		return nil, err
	}
	c := &Corroboration{
		APIVersion: contract.APIVersion,
		Kind:       CorroborationKind,
		Contract:   stA.Contract,
		Period:     stA.Period,
		A:          side(labelA, stA),
		B:          side(labelB, stB),
		Nodes:      []CorroboratedNode{},
	}
	for _, n := range spec.SortedNodes() {
		a, b := setsA[n], setsB[n]
		if len(a) == 0 && len(b) == 0 {
			continue
		}
		both := a.Intersect(b)
		onlyA, onlyB := a.Subtract(b), b.Subtract(a)
		cn := CorroboratedNode{
			Node: n, ASeconds: a.Total(), BSeconds: b.Total(), BothSeconds: both.Total(),
			OnlyASeconds: onlyA.Total(), OnlyBSeconds: onlyB.Total(), OnlyA: spans(onlyA), OnlyB: spans(onlyB),
		}
		c.Summary.BothNodeSeconds += cn.BothSeconds
		c.Summary.OnlyANodeSeconds += cn.OnlyASeconds
		c.Summary.OnlyBNodeSeconds += cn.OnlyBSeconds
		if cn.OnlyASeconds > 0 || cn.OnlyBSeconds > 0 {
			c.Summary.NodesInDispute++
		}
		c.Nodes = append(c.Nodes, cn)
	}
	union := c.Summary.BothNodeSeconds + c.Summary.OnlyANodeSeconds + c.Summary.OnlyBNodeSeconds
	c.Summary.AgreementPercent = "100.00"
	if union > 0 {
		r := new(big.Rat).SetFrac64(c.Summary.BothNodeSeconds*100, union)
		c.Summary.AgreementPercent = r.FloatString(2)
	}
	h, err := canonical.SHA256Hex(c)
	if err != nil {
		return nil, err
	}
	c.ReportSHA256 = h
	return c, nil
}

func side(label string, st *Statement) Side {
	return Side{
		Label: label, StatementSHA256: st.StatementSHA256, EventsSHA256: st.Inputs.EventsSHA256,
		AvailabilityPercent: st.Summary.AvailabilityPercent, DowntimeNodeSeconds: st.Summary.DowntimeNodeSeconds,
		CreditPercent: st.Credit.Percent,
	}
}

func spans(s interval.Set) []Span {
	var out []Span
	for _, iv := range s {
		out = append(out, Span{Start: events.FormatTime(iv.Start), End: events.FormatTime(iv.End), Seconds: iv.Len()})
	}
	return out
}

// SplitBySide divides one event set into the provider's and the tenant's
// under the contract's sources. Maintenance events go to both sides, because
// both parties' views need the maintenance calendar to apply exclusions.
func SplitBySide(spec *contract.Spec, evs []events.Event) (prov, ten []events.Event, err error) {
	if spec.Contract.Sources == nil || spec.Contract.Sources.Tenant == nil {
		return nil, nil, fmt.Errorf("the contract has no sources.tenant, so events cannot be split by party")
	}
	for _, e := range evs {
		side, ok := spec.SideOf(e.SignerRole, e.Source)
		if !ok {
			if spec.IgnoresUnlisted() {
				continue
			}
			return nil, nil, fmt.Errorf("event %s: source %q is not listed in the contract's sources", e.ID, e.Source)
		}
		maint := e.Type == events.MaintenanceStart || e.Type == events.MaintenanceEnd
		if side == contract.SideTenant || maint {
			ten = append(ten, e)
		}
		if side == contract.SideProvider || maint {
			prov = append(prov, e)
		}
	}
	return prov, ten, nil
}

// DowntimeSets rebuilds each node's countable downtime from the incidents.
func (s *Statement) DowntimeSets() (map[string]interval.Set, error) {
	out := map[string]interval.Set{}
	for _, n := range s.Nodes {
		var ivs []interval.Interval
		for _, in := range n.Incidents {
			a, err := events.ParseTime(in.Start)
			if err != nil {
				return nil, err
			}
			b, err := events.ParseTime(in.End)
			if err != nil {
				return nil, err
			}
			ivs = append(ivs, interval.Interval{Start: a, End: b})
		}
		if set := interval.Normalize(ivs); len(set) > 0 {
			out[n.Node] = set
		}
	}
	return out, nil
}

// Text renders a short human-readable comparison.
func (c *Corroboration) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Contract   %s (version %d), %s to %s\n", c.Contract.ID, c.Contract.Version, c.Period.Start, c.Period.End)
	fmt.Fprintf(&b, "%-10s availability %s%%, downtime %d node-seconds, credit %d%%\n", c.A.Label, c.A.AvailabilityPercent, c.A.DowntimeNodeSeconds, c.A.CreditPercent)
	fmt.Fprintf(&b, "%-10s availability %s%%, downtime %d node-seconds, credit %d%%\n", c.B.Label, c.B.AvailabilityPercent, c.B.DowntimeNodeSeconds, c.B.CreditPercent)
	fmt.Fprintf(&b, "Agreement  %s%% of all recorded downtime is seen by both; %d node(s) in dispute\n", c.Summary.AgreementPercent, c.Summary.NodesInDispute)
	if len(c.Nodes) > 0 {
		fmt.Fprintf(&b, "\n%-15s %10s %10s %10s %12s %12s\n", "Node", c.A.Label, c.B.Label, "both", "only "+c.A.Label, "only "+c.B.Label)
		for _, n := range c.Nodes {
			fmt.Fprintf(&b, "%-15s %10d %10d %10d %12d %12d\n", n.Node, n.ASeconds, n.BSeconds, n.BothSeconds, n.OnlyASeconds, n.OnlyBSeconds)
		}
	}
	fmt.Fprintf(&b, "\nreportSha256 %s\n", c.ReportSHA256)
	return b.String()
}
