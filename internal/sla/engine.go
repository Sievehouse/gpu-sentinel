// Package sla turns availability events into a sealed SLA statement.
//
// The computation is deterministic: the same contract, events and period
// always produce byte-identical canonical output and the same statement hash.
// There is no floating point anywhere in the money path.
//
// Order of operations, per node:
//
//  1. Replay the node's events in order to build down, degraded and
//     maintenance intervals, clipped to the period.
//  2. Classify intervals: customer-caused downtime and maintenance announced
//     with enough notice are excluded; everything else is countable.
//  3. Apply the per-node exclusion cap: excluded time beyond the cap counts.
//  4. Countable = (countable minus allowed exclusions) plus cap overflow,
//     then drop outages shorter than the contract's minimum incident length.
//  5. Rack rule: while at least N nodes of a rack have countable downtime,
//     every node of the rack is down.
//  6. Sum node downtime, compute availability as an exact fraction and pick
//     the highest credit tier whose threshold is above availability.
//
// Unmonitored time (stretches with no heartbeat, derived from evidence logs)
// is always reported. If the contract says "count-as-down", it is also
// countable downtime and goes through steps 2 to 5 like any outage.
package sla

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/Sievehouse/capacity-ledger/internal/contract"
	"github.com/Sievehouse/capacity-ledger/internal/events"
	"github.com/Sievehouse/capacity-ledger/internal/interval"
)

// EngineVersion is the version of this engine. Bump it whenever the
// computation changes.
const EngineVersion = "0.2.0"

// Semantics versions. A statement's engineVersion field records the oldest
// semantics that reproduces it: inputs that use no 0.2 feature (unmonitored
// time) give statements byte-identical to engine 0.1.0, so statements and
// hashes published with v0.1 still replay.
const (
	Semantics010 = "0.1.0"
	Semantics020 = "0.2.0"
)

// Supported reports whether this engine can replay statements of semantics v.
func Supported(v string) bool { return v == Semantics010 || v == Semantics020 }

// StatementKind identifies a statement document.
const StatementKind = "SLAStatement"

// Period is the measurement period [Start, End) in Unix seconds.
type Period struct {
	Start int64
	End   int64
}

// Validate checks that the period is non-empty.
func (p Period) Validate() error {
	if p.End <= p.Start {
		return fmt.Errorf("period end must be after start")
	}
	return nil
}

type segment struct {
	iv         interval.Interval
	kind       string // down | degraded | maintenance | unmonitored
	causes     []string
	ids        []string
	excluded   bool
	reason     string
	reportOnly bool // unmonitored time under the "report" policy
}

type warnCounts struct {
	unknownNodes     map[string]int
	upWithoutDown    int
	dupDown          int
	degEndWithout    int
	dupDegraded      int
	maintEndWithout  int
	dupMaintenance   int
	maintNoNotice    int
	degradedIgnored  int
	unknownNodeTotal int
	unmonEndWithout  int
	dupUnmonitored   int
}

// Compute builds and seals the statement for one contract and period.
// Events must be normalised (events.ReadJSONL does this).
func Compute(spec *contract.Spec, evs []events.Event, p Period) (*Statement, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	contractDigest, err := spec.Digest()
	if err != nil {
		return nil, err
	}
	sorted := append([]events.Event(nil), evs...)
	events.Sort(sorted)
	eventsDigest, err := events.Digest(sorted)
	if err != nil {
		return nil, err
	}

	w := &warnCounts{unknownNodes: map[string]int{}}
	byNode := map[string][]events.Event{}
	for _, e := range sorted {
		if !spec.HasNode(e.Node) {
			w.unknownNodes[e.Node]++
			w.unknownNodeTotal++
			continue
		}
		byNode[e.Node] = append(byNode[e.Node], e)
	}

	nodes := spec.SortedNodes()
	segs := make(map[string][]segment, len(nodes))
	nodeDown := make(map[string]interval.Set, len(nodes))
	allowedExcl := make(map[string]interval.Set, len(nodes))
	unmonitored := make(map[string]interval.Set, len(nodes))
	for _, n := range nodes {
		segs[n] = buildSegments(spec, byNode[n], p, w)
		var countable, excluded, unmon []interval.Interval
		for _, sg := range segs[n] {
			if sg.kind == "unmonitored" {
				unmon = append(unmon, sg.iv)
			}
			if sg.reportOnly {
				continue
			}
			if sg.excluded {
				excluded = append(excluded, sg.iv)
			} else {
				countable = append(countable, sg.iv)
			}
		}
		allowed, overflow := interval.Normalize(excluded).SplitAfter(spec.ExclusionCap)
		down := interval.Normalize(countable).Subtract(allowed).Union(overflow).DropShorterThan(spec.MinIncident)
		nodeDown[n] = down
		allowedExcl[n] = allowed
		unmonitored[n] = interval.Normalize(unmon)
	}

	final := make(map[string]interval.Set, len(nodes))
	rackOnly := map[string]interval.Set{}
	for _, n := range nodes {
		final[n] = nodeDown[n]
	}
	if rr := spec.Contract.Measurement.RackRule; rr != nil {
		for _, rack := range spec.SortedRacks() {
			sets := make([]interval.Set, 0, len(rack.Nodes))
			for _, n := range rack.Nodes {
				sets = append(sets, nodeDown[n])
			}
			rackDown := interval.AtLeast(sets, rr.NodesDownThreshold).DropShorterThan(spec.MinIncident)
			if len(rackDown) == 0 {
				continue
			}
			for _, n := range rack.Nodes {
				final[n] = final[n].Union(rackDown)
				rackOnly[n] = rackDown.Subtract(nodeDown[n])
			}
		}
	}

	st := &Statement{
		APIVersion: contract.APIVersion,
		Kind:       StatementKind,
		Contract: ContractRef{
			ID:       spec.Contract.Metadata.ID,
			Version:  spec.Contract.Metadata.Version,
			Provider: spec.Contract.Metadata.Provider,
			Customer: spec.Contract.Metadata.Customer,
		},
		Period: PeriodRef{Start: events.FormatTime(p.Start), End: events.FormatTime(p.End), Seconds: p.End - p.Start},
		Inputs: Inputs{ContractSHA256: contractDigest, EventsSHA256: eventsDigest, EventCount: len(sorted)},
		Nodes:  []NodeResult{},
	}

	var downTotal, exclTotal, unmonTotal int64
	for _, n := range nodes {
		nr := NodeResult{Node: n, DowntimeSeconds: final[n].Total(), ExcludedSeconds: allowedExcl[n].Total(), UnmonitoredSeconds: unmonitored[n].Total()}
		if r, ok := spec.RackOf(n); ok {
			nr.Rack = r
		}
		for _, iv := range nodeDown[n] {
			causes, ids := attribute(iv, segs[n], false)
			nr.Incidents = append(nr.Incidents, incident(iv, "node", causes, ids))
		}
		if ro := rackOnly[n]; len(ro) > 0 {
			rackName := nr.Rack
			for _, iv := range ro {
				var ids []string
				for _, peer := range rackNodes(spec, rackName) {
					_, peerIDs := attribute(iv, segs[peer], true)
					ids = append(ids, peerIDs...)
				}
				nr.Incidents = append(nr.Incidents, incident(iv, "rack:"+rackName, []string{"rack-rule"}, uniqueSorted(ids)))
			}
		}
		sort.SliceStable(nr.Incidents, func(i, j int) bool {
			if nr.Incidents[i].Start != nr.Incidents[j].Start {
				return nr.Incidents[i].Start < nr.Incidents[j].Start
			}
			return nr.Incidents[i].Via < nr.Incidents[j].Via
		})
		for _, iv := range allowedExcl[n] {
			reasons, ids := exclusionReasons(iv, segs[n])
			nr.Exclusions = append(nr.Exclusions, Exclusion{
				Start: events.FormatTime(iv.Start), End: events.FormatTime(iv.End),
				Seconds: iv.Len(), Reasons: reasons, EventIDs: ids,
			})
		}
		for _, iv := range unmonitored[n] {
			var ids []string
			for _, sg := range segs[n] {
				if sg.kind == "unmonitored" && sg.iv.Overlaps(iv) {
					ids = append(ids, sg.ids...)
				}
			}
			nr.Unmonitored = append(nr.Unmonitored, Span{
				Start: events.FormatTime(iv.Start), End: events.FormatTime(iv.End),
				Seconds: iv.Len(), EventIDs: uniqueSorted(ids),
			})
		}
		downTotal += nr.DowntimeSeconds
		exclTotal += nr.ExcludedSeconds
		unmonTotal += nr.UnmonitoredSeconds
		if nr.DowntimeSeconds > 0 || nr.ExcludedSeconds > 0 || nr.UnmonitoredSeconds > 0 {
			st.Nodes = append(st.Nodes, nr)
		}
		if nr.DowntimeSeconds > 0 {
			st.Summary.NodesWithDowntime++
		}
	}

	gpus := int64(spec.Contract.Cluster.GPUsPerNode)
	total := int64(len(nodes)) * (p.End - p.Start)
	availPct := new(big.Rat).SetFrac64(total-downTotal, total)
	availPct.Mul(availPct, big.NewRat(100, 1))
	st.Summary.Nodes = len(nodes)
	st.Summary.GPUsPerNode = int(gpus)
	st.Summary.NodeSecondsTotal = total
	st.Summary.DowntimeNodeSeconds = downTotal
	st.Summary.ExcludedNodeSeconds = exclTotal
	st.Summary.DowntimeGPUHours = new(big.Rat).SetFrac64(downTotal*gpus, 3600).FloatString(2)
	st.Summary.AvailabilityPercent = availPct.FloatString(4)
	st.Summary.UnmonitoredNodeSeconds = unmonTotal
	st.Summary.UnmonitoredPolicy = spec.Contract.Measurement.Unmonitored
	st.Credit = credit(spec, availPct)
	st.Warnings = w.render()
	st.EngineVersion = Semantics010
	if unmonTotal > 0 || spec.Contract.Measurement.Unmonitored != "" {
		st.EngineVersion = Semantics020
	}

	if err := st.Seal(); err != nil {
		return nil, err
	}
	return st, nil
}

// buildSegments replays one node's events into classified intervals.
func buildSegments(spec *contract.Spec, evs []events.Event, p Period, w *warnCounts) []segment {
	type openState struct {
		start     int64
		causes    []string
		ids       []string
		notice    int64
		hasNotice bool
	}
	var down, deg, maint, unmon *openState
	var out []segment

	closeSeg := func(o *openState, end int64, kind, endID string) {
		ids := append([]string(nil), o.ids...)
		if endID != "" {
			ids = append(ids, endID)
		}
		iv := interval.Interval{Start: max(o.start, p.Start), End: min(end, p.End)}
		if iv.End <= iv.Start {
			return
		}
		sg := segment{iv: iv, kind: kind, causes: uniqueSorted(o.causes), ids: ids}
		switch kind {
		case "down":
			if spec.Contract.Exclusions.CustomerCaused && allCustomer(sg.causes) {
				sg.excluded, sg.reason = true, "customer-caused"
			}
		case "degraded":
			if !spec.Contract.Measurement.DegradedCountsAsDown {
				w.degradedIgnored++
				return
			}
		case "maintenance":
			if spec.MaintenanceExcludable && o.hasNotice && o.start-o.notice >= spec.MaintenanceNotice {
				sg.excluded, sg.reason = true, "scheduled-maintenance"
			} else {
				sg.reason = "unannounced-maintenance"
				if !o.hasNotice {
					w.maintNoNotice++
				}
			}
		case "unmonitored":
			sg.reportOnly = !spec.CountsUnmonitored()
		}
		out = append(out, sg)
	}

	for i := range evs {
		e := &evs[i]
		u := e.Unix()
		switch e.Type {
		case events.Down:
			if down == nil {
				down = &openState{start: u}
			} else {
				w.dupDown++
			}
			down.causes = appendCause(down.causes, e.Cause)
			down.ids = append(down.ids, e.ID)
		case events.Up:
			if down == nil {
				w.upWithoutDown++
				continue
			}
			closeSeg(down, u, "down", e.ID)
			down = nil
		case events.DegradedStart:
			if deg == nil {
				deg = &openState{start: u}
			} else {
				w.dupDegraded++
			}
			deg.causes = appendCause(deg.causes, e.Cause)
			deg.ids = append(deg.ids, e.ID)
		case events.DegradedEnd:
			if deg == nil {
				w.degEndWithout++
				continue
			}
			closeSeg(deg, u, "degraded", e.ID)
			deg = nil
		case events.MaintenanceStart:
			if maint == nil {
				n, ok := e.Notice()
				maint = &openState{start: u, notice: n, hasNotice: ok}
			} else {
				w.dupMaintenance++
			}
			cause := e.Cause
			if cause == "" {
				cause = "maintenance"
			}
			maint.causes = appendCause(maint.causes, cause)
			maint.ids = append(maint.ids, e.ID)
		case events.MaintenanceEnd:
			if maint == nil {
				w.maintEndWithout++
				continue
			}
			closeSeg(maint, u, "maintenance", e.ID)
			maint = nil
		case events.UnmonitoredStart:
			if unmon == nil {
				unmon = &openState{start: u}
			} else {
				w.dupUnmonitored++
			}
			cause := e.Cause
			if cause == "" {
				cause = "unmonitored"
			}
			unmon.causes = appendCause(unmon.causes, cause)
			unmon.ids = append(unmon.ids, e.ID)
		case events.UnmonitoredEnd:
			if unmon == nil {
				w.unmonEndWithout++
				continue
			}
			closeSeg(unmon, u, "unmonitored", e.ID)
			unmon = nil
		}
	}
	// Anything still open runs to the end of the period.
	if down != nil {
		closeSeg(down, p.End, "down", "")
	}
	if deg != nil {
		closeSeg(deg, p.End, "degraded", "")
	}
	if maint != nil {
		closeSeg(maint, p.End, "maintenance", "")
	}
	if unmon != nil {
		closeSeg(unmon, p.End, "unmonitored", "")
	}
	return out
}

// attribute collects causes and event ids from segments that overlap iv. With
// countableOnly set, excluded segments are skipped.
func attribute(iv interval.Interval, segs []segment, countableOnly bool) (causes, ids []string) {
	for _, sg := range segs {
		if !sg.iv.Overlaps(iv) || (countableOnly && sg.excluded) {
			continue
		}
		causes = append(causes, sg.causes...)
		if sg.kind == "maintenance" && !sg.excluded {
			causes = append(causes, sg.reason)
		}
		if sg.excluded {
			causes = append(causes, "exclusion-cap-exceeded")
		}
		ids = append(ids, sg.ids...)
	}
	return uniqueSorted(causes), uniqueSorted(ids)
}

func exclusionReasons(iv interval.Interval, segs []segment) (reasons, ids []string) {
	for _, sg := range segs {
		if sg.excluded && sg.iv.Overlaps(iv) {
			reasons = append(reasons, sg.reason)
			ids = append(ids, sg.ids...)
		}
	}
	return uniqueSorted(reasons), uniqueSorted(ids)
}

func incident(iv interval.Interval, via string, causes, ids []string) Incident {
	return Incident{
		Start: events.FormatTime(iv.Start), End: events.FormatTime(iv.End),
		Seconds: iv.Len(), Via: via, Causes: causes, EventIDs: ids,
	}
}

func rackNodes(spec *contract.Spec, name string) []string {
	for _, r := range spec.Contract.Cluster.Racks {
		if r.Name == name {
			return r.Nodes
		}
	}
	return nil
}

// credit picks the highest tier whose threshold is above availability.
func credit(spec *contract.Spec, availPct *big.Rat) Credit {
	c := Credit{Currency: spec.Contract.Credits.Currency}
	var best *contract.ParsedTier
	for i := range spec.Tiers {
		t := &spec.Tiers[i]
		if availPct.Cmp(t.Below) >= 0 {
			continue
		}
		if best == nil || t.Percent > best.Percent || (t.Percent == best.Percent && t.Below.Cmp(best.Below) < 0) {
			best = t
		}
	}
	if best != nil {
		c.Percent = best.Percent
		c.TierAvailabilityBelow = best.BelowText
	}
	if cp := spec.Contract.Credits.CapPercent; cp > 0 && c.Percent > cp {
		c.Percent, c.Capped = cp, true
	}
	if spec.Fee != nil {
		amt := new(big.Rat).Mul(spec.Fee, big.NewRat(int64(c.Percent), 100))
		c.Amount = amt.FloatString(2)
	}
	return c
}

func (w *warnCounts) render() []string {
	out := []string{}
	if w.unknownNodeTotal > 0 {
		names := make([]string, 0, len(w.unknownNodes))
		for n := range w.unknownNodes {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) > 5 {
			names = append(names[:5], "...")
		}
		out = append(out, fmt.Sprintf("%d event(s) for nodes not in the contract were ignored: %s", w.unknownNodeTotal, strings.Join(names, ", ")))
	}
	add := func(n int, msg string) {
		if n > 0 {
			out = append(out, fmt.Sprintf("%d %s", n, msg))
		}
	}
	add(w.upWithoutDown, "up event(s) without a preceding down were ignored")
	add(w.dupDown, "down event(s) for nodes already down were merged into the open incident")
	add(w.degEndWithout, "degraded_end event(s) without a preceding degraded_start were ignored")
	add(w.dupDegraded, "degraded_start event(s) for nodes already degraded were merged")
	add(w.maintEndWithout, "maintenance_end event(s) without a preceding maintenance_start were ignored")
	add(w.dupMaintenance, "maintenance_start event(s) during open maintenance were merged")
	add(w.maintNoNotice, "maintenance window(s) without noticeTs were treated as unannounced")
	add(w.degradedIgnored, "degraded interval(s) ignored because the contract does not count degradation as downtime")
	add(w.unmonEndWithout, "unmonitored_end event(s) without a preceding unmonitored_start were ignored")
	add(w.dupUnmonitored, "unmonitored_start event(s) during an open unmonitored stretch were merged")
	return out
}

func appendCause(causes []string, cause string) []string {
	if cause == "" {
		return causes
	}
	return append(causes, cause)
}

func allCustomer(causes []string) bool {
	if len(causes) == 0 {
		return false
	}
	for _, c := range causes {
		if !strings.HasPrefix(c, "customer") {
			return false
		}
	}
	return true
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// Replay recomputes a statement from its inputs and checks that it matches,
// including the contract and event digests and the statement hash.
func Replay(spec *contract.Spec, evs []events.Event, stored *Statement) error {
	if err := stored.CheckSeal(); err != nil {
		return err
	}
	if !Supported(stored.EngineVersion) {
		return fmt.Errorf("statement needs engine semantics %s; this binary (engine %s) supports %s and %s",
			stored.EngineVersion, EngineVersion, Semantics010, Semantics020)
	}
	start, err := events.ParseTime(stored.Period.Start)
	if err != nil {
		return fmt.Errorf("period start: %w", err)
	}
	end, err := events.ParseTime(stored.Period.End)
	if err != nil {
		return fmt.Errorf("period end: %w", err)
	}
	fresh, err := Compute(spec, evs, Period{Start: start, End: end})
	if err != nil {
		return err
	}
	if fresh.Inputs.ContractSHA256 != stored.Inputs.ContractSHA256 {
		return fmt.Errorf("contract differs from the one the statement was computed with")
	}
	if fresh.Inputs.EventsSHA256 != stored.Inputs.EventsSHA256 {
		return fmt.Errorf("events differ from the ones the statement was computed with")
	}
	if fresh.StatementSHA256 != stored.StatementSHA256 {
		return fmt.Errorf("replayed statement %s does not match stored %s", fresh.StatementSHA256, stored.StatementSHA256)
	}
	return nil
}
