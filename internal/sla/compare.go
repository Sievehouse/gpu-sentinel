package sla

import (
	"bufio"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/Sievehouse/gpu-sentinel/internal/contract"
)

// ProviderReport is the downtime a provider reports by its own means (its
// status page, its SLA report, its monitoring), per node, in seconds. A
// pilot compares it with a statement computed from signed evidence.
type ProviderReport map[string]int64

// ReadProviderReport reads a CSV with a node and its downtime in seconds per
// line. The first non-comment line may be a header (its second field is not
// a number); blank lines and lines starting with # are skipped. A node listed twice is an
// error.
func ReadProviderReport(r io.Reader) (ProviderReport, error) {
	out := ProviderReport{}
	sc := bufio.NewScanner(r)
	n, first := 0, true
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		header := first
		first = false
		f := strings.Split(line, ",")
		if len(f) < 2 {
			return nil, fmt.Errorf("line %d: want node,downtime_seconds", n)
		}
		node, val := strings.TrimSpace(f[0]), strings.TrimSpace(f[1])
		secs, err := strconv.ParseInt(val, 10, 64)
		if err != nil {
			if header {
				continue
			}
			return nil, fmt.Errorf("line %d: downtime %q is not whole seconds", n, val)
		}
		if secs < 0 {
			return nil, fmt.Errorf("line %d: negative downtime", n)
		}
		if _, dup := out[node]; dup {
			return nil, fmt.Errorf("line %d: node %s listed twice", n, node)
		}
		out[node] = secs
	}
	return out, sc.Err()
}

// NodeComparison is one node's downtime in the statement and in the
// provider's report.
type NodeComparison struct {
	Node      string `json:"node"`
	Statement int64  `json:"statementSeconds"`
	Provider  int64  `json:"providerSeconds"`
	// Difference is provider minus statement: negative when the provider
	// reports less downtime than the evidence shows.
	Difference int64 `json:"differenceSeconds"`
	Flagged    bool  `json:"flagged"`
}

// Comparison is a pilot's result for one period.
type Comparison struct {
	Contract              string           `json:"contract"`
	Period                PeriodRef        `json:"period"`
	StatementSHA256       string           `json:"statementSha256"`
	ToleranceSeconds      int64            `json:"toleranceSeconds"`
	StatementDowntime     int64            `json:"statementDowntimeSeconds"`
	ProviderDowntime      int64            `json:"providerDowntimeSeconds"`
	StatementAvailability string           `json:"statementAvailabilityPercent"`
	ProviderAvailability  string           `json:"providerAvailabilityPercent"`
	StatementCredit       Credit           `json:"statementCredit"`
	ProviderCredit        *Credit          `json:"providerCredit,omitempty"`
	Nodes                 []NodeComparison `json:"nodes"`
	Flagged               int              `json:"flaggedNodes"`
	UnknownNodes          []string         `json:"unknownNodes"`
}

// Compare compares a statement with the provider's own report. Nodes whose
// figures differ by more than tolerance seconds are flagged. With spec (the
// statement's contract), the credit the provider's figures imply is worked
// out too.
func Compare(st *Statement, rep ProviderReport, tolerance int64, spec *contract.Spec) (*Comparison, error) {
	if err := st.CheckSeal(); err != nil {
		return nil, err
	}
	if spec != nil && spec.Contract.Metadata.ID != st.Contract.ID {
		return nil, fmt.Errorf("the statement is for contract %s, not %s", st.Contract.ID, spec.Contract.Metadata.ID)
	}
	c := &Comparison{
		Contract: st.Contract.ID, Period: st.Period, StatementSHA256: st.StatementSHA256, ToleranceSeconds: tolerance,
		StatementDowntime: st.Summary.DowntimeNodeSeconds, StatementAvailability: st.Summary.AvailabilityPercent,
		StatementCredit: st.Credit, Nodes: []NodeComparison{}, UnknownNodes: []string{},
	}
	ours := map[string]int64{}
	for _, n := range st.Nodes {
		ours[n.Node] = n.DowntimeSeconds
	}
	known := map[string]bool{}
	if spec != nil {
		for _, n := range spec.SortedNodes() {
			known[n] = true
		}
	}
	names := map[string]bool{}
	for n := range ours {
		names[n] = true
	}
	for n, secs := range rep {
		if spec != nil && !known[n] {
			c.UnknownNodes = append(c.UnknownNodes, n)
			continue
		}
		names[n] = true
		c.ProviderDowntime += secs
	}
	sort.Strings(c.UnknownNodes)
	list := make([]string, 0, len(names))
	for n := range names {
		list = append(list, n)
	}
	sort.Strings(list)
	for _, n := range list {
		nc := NodeComparison{Node: n, Statement: ours[n], Provider: rep[n]}
		nc.Difference = nc.Provider - nc.Statement
		if nc.Difference > tolerance || -nc.Difference > tolerance {
			nc.Flagged = true
			c.Flagged++
		}
		if nc.Statement == 0 && nc.Provider == 0 {
			continue
		}
		c.Nodes = append(c.Nodes, nc)
	}
	total := st.Summary.NodeSecondsTotal
	if total <= 0 {
		return nil, fmt.Errorf("statement has no node-seconds")
	}
	if c.ProviderDowntime > total {
		return nil, fmt.Errorf("the provider reports %d node-seconds of downtime, more than the period holds", c.ProviderDowntime)
	}
	avail := new(big.Rat).SetFrac64(total-c.ProviderDowntime, total)
	avail.Mul(avail, big.NewRat(100, 1))
	c.ProviderAvailability = avail.FloatString(4)
	if spec != nil {
		pc := credit(spec, avail)
		c.ProviderCredit = &pc
	}
	return c, nil
}

// Text renders the comparison for people.
func (c *Comparison) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Contract    %s, %s to %s\n", c.Contract, c.Period.Start, c.Period.End)
	fmt.Fprintf(&b, "statement   availability %s%%, downtime %d node-seconds, credit %s\n", c.StatementAvailability, c.StatementDowntime, creditText(&c.StatementCredit))
	pc := "(give --contract to work out the credit)"
	if c.ProviderCredit != nil {
		pc = creditText(c.ProviderCredit)
	}
	fmt.Fprintf(&b, "provider    availability %s%%, downtime %d node-seconds, credit %s\n", c.ProviderAvailability, c.ProviderDowntime, pc)
	fmt.Fprintf(&b, "Flagged     %d node(s) differ by more than %d s\n\n", c.Flagged, c.ToleranceSeconds)
	fmt.Fprintf(&b, "%-14s %10s %10s %11s\n", "Node", "statement", "provider", "difference")
	for _, n := range c.Nodes {
		flag := ""
		if n.Flagged {
			flag = "  <-"
		}
		fmt.Fprintf(&b, "%-14s %10d %10d %+11d%s\n", n.Node, n.Statement, n.Provider, n.Difference, flag)
	}
	if len(c.UnknownNodes) > 0 {
		fmt.Fprintf(&b, "\nnot in the contract, ignored: %s\n", strings.Join(c.UnknownNodes, ", "))
	}
	return b.String()
}

func creditText(c *Credit) string {
	s := fmt.Sprintf("%d%%", c.Percent)
	if c.Amount != "" {
		s += " = " + c.Amount + " " + c.Currency
	}
	return s
}
