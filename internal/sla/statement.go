package sla

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/Sievehouse/capacity-ledger/internal/canonical"
)

// Statement is the output of one SLA computation. It is sealed with a
// SHA-256 over its canonical encoding, so either contract party can replay
// the inputs and check that they get the same statement.
type Statement struct {
	APIVersion      string       `json:"apiVersion"`
	Kind            string       `json:"kind"`
	EngineVersion   string       `json:"engineVersion"`
	Contract        ContractRef  `json:"contract"`
	Period          PeriodRef    `json:"period"`
	Inputs          Inputs       `json:"inputs"`
	Summary         Summary      `json:"summary"`
	Credit          Credit       `json:"credit"`
	Nodes           []NodeResult `json:"nodes"`
	Warnings        []string     `json:"warnings"`
	StatementSHA256 string       `json:"statementSha256"`
}

// ContractRef identifies the contract the statement was computed under.
type ContractRef struct {
	ID       string `json:"id"`
	Version  int    `json:"version"`
	Provider string `json:"provider,omitempty"`
	Customer string `json:"customer,omitempty"`
}

// PeriodRef is the measurement period [start, end).
type PeriodRef struct {
	Start   string `json:"start"`
	End     string `json:"end"`
	Seconds int64  `json:"seconds"`
}

// Inputs fingerprints everything the statement depends on.
type Inputs struct {
	ContractSHA256 string `json:"contractSha256"`
	EventsSHA256   string `json:"eventsSha256"`
	EventCount     int    `json:"eventCount"`
}

// Summary holds cluster-level results. Decimal values are strings so that
// the hash never depends on floating-point formatting.
type Summary struct {
	Nodes               int    `json:"nodes"`
	GPUsPerNode         int    `json:"gpusPerNode"`
	NodeSecondsTotal    int64  `json:"nodeSecondsTotal"`
	DowntimeNodeSeconds int64  `json:"downtimeNodeSeconds"`
	ExcludedNodeSeconds int64  `json:"excludedNodeSeconds"`
	DowntimeGPUHours    string `json:"downtimeGpuHours"`
	AvailabilityPercent string `json:"availabilityPercent"`
	NodesWithDowntime   int    `json:"nodesWithDowntime"`
	// Since 0.2: time without heartbeats, and the contract's policy for it.
	UnmonitoredNodeSeconds int64  `json:"unmonitoredNodeSeconds,omitempty"`
	UnmonitoredPolicy      string `json:"unmonitoredPolicy,omitempty"`
}

// Credit is the service credit owed for the period.
type Credit struct {
	Percent               int    `json:"percent"`
	TierAvailabilityBelow string `json:"tierAvailabilityBelow,omitempty"`
	Capped                bool   `json:"capped"`
	Amount                string `json:"amount,omitempty"`
	Currency              string `json:"currency,omitempty"`
}

// NodeResult lists countable downtime and exclusions for one node. Nodes
// with neither are omitted from the statement.
type NodeResult struct {
	Node            string      `json:"node"`
	Rack            string      `json:"rack,omitempty"`
	DowntimeSeconds int64       `json:"downtimeSeconds"`
	ExcludedSeconds int64       `json:"excludedSeconds"`
	Incidents       []Incident  `json:"incidents,omitempty"`
	Exclusions      []Exclusion `json:"exclusions,omitempty"`
	// Since 0.2: stretches with no heartbeat, whatever the policy.
	UnmonitoredSeconds int64  `json:"unmonitoredSeconds,omitempty"`
	Unmonitored        []Span `json:"unmonitored,omitempty"`
}

// Span is a stretch of time with the events that bound it.
type Span struct {
	Start    string   `json:"start"`
	End      string   `json:"end"`
	Seconds  int64    `json:"seconds"`
	EventIDs []string `json:"eventIds,omitempty"`
}

// Incident is one countable stretch of downtime. Via is "node" when the node
// itself was unavailable and "rack:<name>" when the rack rule applied.
type Incident struct {
	Start    string   `json:"start"`
	End      string   `json:"end"`
	Seconds  int64    `json:"seconds"`
	Via      string   `json:"via"`
	Causes   []string `json:"causes,omitempty"`
	EventIDs []string `json:"eventIds,omitempty"`
}

// Exclusion is one stretch of unavailability that does not count.
type Exclusion struct {
	Start    string   `json:"start"`
	End      string   `json:"end"`
	Seconds  int64    `json:"seconds"`
	Reasons  []string `json:"reasons"`
	EventIDs []string `json:"eventIds,omitempty"`
}

// ComputeHash returns the SHA-256 of the canonical statement with the
// statementSha256 field emptied.
func (s *Statement) ComputeHash() (string, error) {
	cp := *s
	cp.StatementSHA256 = ""
	return canonical.SHA256Hex(cp)
}

// Seal sets StatementSHA256.
func (s *Statement) Seal() error {
	h, err := s.ComputeHash()
	if err != nil {
		return err
	}
	s.StatementSHA256 = h
	return nil
}

// CheckSeal reports whether the stored hash matches the content.
func (s *Statement) CheckSeal() error {
	h, err := s.ComputeHash()
	if err != nil {
		return err
	}
	if h != s.StatementSHA256 {
		return fmt.Errorf("statement hash mismatch: stored %s, computed %s", s.StatementSHA256, h)
	}
	return nil
}

// ReadStatement loads a statement file. Unknown fields are rejected so that
// content added after sealing cannot pass verification unnoticed.
func ReadStatement(path string) (*Statement, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s Statement
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &s, nil
}

// Text renders a short human-readable summary.
func (s *Statement) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Contract      %s (version %d)\n", s.Contract.ID, s.Contract.Version)
	fmt.Fprintf(&b, "Period        %s to %s\n", s.Period.Start, s.Period.End)
	fmt.Fprintf(&b, "Nodes         %d x %d GPUs\n", s.Summary.Nodes, s.Summary.GPUsPerNode)
	fmt.Fprintf(&b, "Availability  %s%%\n", s.Summary.AvailabilityPercent)
	fmt.Fprintf(&b, "Downtime      %s GPU-hours across %d node(s); %d node-seconds excluded\n",
		s.Summary.DowntimeGPUHours, s.Summary.NodesWithDowntime, s.Summary.ExcludedNodeSeconds)
	credit := fmt.Sprintf("%d%%", s.Credit.Percent)
	if s.Credit.TierAvailabilityBelow != "" {
		credit += fmt.Sprintf(" (availability below %s%%)", s.Credit.TierAvailabilityBelow)
	}
	if s.Credit.Capped {
		credit += ", capped"
	}
	if s.Credit.Amount != "" {
		credit += fmt.Sprintf(" = %s %s", s.Credit.Amount, s.Credit.Currency)
	}
	fmt.Fprintf(&b, "Credit        %s\n", credit)
	if s.Summary.UnmonitoredNodeSeconds > 0 || s.Summary.UnmonitoredPolicy != "" {
		policy := s.Summary.UnmonitoredPolicy
		if policy == "" {
			policy = "report"
		}
		fmt.Fprintf(&b, "Unmonitored   %d node-seconds without heartbeats (policy: %s)\n", s.Summary.UnmonitoredNodeSeconds, policy)
	}
	if len(s.Nodes) > 0 {
		b.WriteString("\nNode            Downtime(s)  Excluded(s)  Incidents\n")
		for _, n := range s.Nodes {
			var parts []string
			for _, in := range n.Incidents {
				parts = append(parts, fmt.Sprintf("%s %ds [%s]", in.Start, in.Seconds, strings.Join(append([]string{in.Via}, in.Causes...), " ")))
			}
			if n.UnmonitoredSeconds > 0 {
				parts = append(parts, fmt.Sprintf("unmonitored %ds", n.UnmonitoredSeconds))
			}
			line := fmt.Sprintf("%-15s %11d  %11d  %s", n.Node, n.DowntimeSeconds, n.ExcludedSeconds, strings.Join(parts, "; "))
			b.WriteString(strings.TrimRight(line, " ") + "\n")
		}
	}
	for _, w := range s.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	fmt.Fprintf(&b, "\nstatementSha256 %s\n", s.StatementSHA256)
	return b.String()
}
