// Package xid parses GPU fault messages from the kernel log (NVIDIA XID,
// NVSwitch SXID and AMD amdgpu RAS, reset and timeout messages) and AMD RAS
// error counters, and maps them to SLA actions through a reviewable policy.
package xid

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Observation is one parsed GPU fault message or counter change.
type Observation struct {
	Kind    string `json:"kind"`            // "xid", "sxid" or "amdgpu"
	Code    int    `json:"code,omitempty"`  // XID or SXID code
	Class   string `json:"class,omitempty"` // amdgpu class, e.g. "ras_uncorrectable"
	Block   string `json:"block,omitempty"` // amdgpu RAS block, e.g. "umc"
	Count   int64  `json:"count,omitempty"` // amdgpu error count
	Device  string `json:"device"`          // PCI address as printed by the driver
	Message string `json:"message"`
	TS      string `json:"ts,omitempty"` // RFC 3339, when the log line carried a timestamp
	Raw     string `json:"raw,omitempty"`
}

var (
	xidRe  = regexp.MustCompile(`NVRM: Xid \((?:PCI:)?([0-9A-Fa-f:.]+)\): (\d+),\s*(.*)$`)
	sxidRe = regexp.MustCompile(`SXid \((?:PCI:)?([0-9A-Fa-f:.]+)\): (\d+),\s*(.*)$`)
	// journalctl -o short-iso and similar formats put an ISO timestamp first.
	tsRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2}))\s`)
)

// ParseLine extracts an XID or SXID observation from one kernel log line. It
// accepts /dev/kmsg records, dmesg output and journald exports.
func ParseLine(line string) (Observation, bool) {
	line = strings.TrimRight(line, "\r\n")
	if o, ok := parseAMDLine(line); ok {
		return o, true
	}
	kind := "xid"
	m := xidRe.FindStringSubmatch(line)
	if m == nil {
		kind = "sxid"
		m = sxidRe.FindStringSubmatch(line)
	}
	if m == nil {
		return Observation{}, false
	}
	code, err := strconv.Atoi(m[2])
	if err != nil {
		return Observation{}, false
	}
	o := Observation{Kind: kind, Code: code, Device: m[1], Message: strings.TrimSpace(m[3]), Raw: line}
	if tm := tsRe.FindStringSubmatch(line); tm != nil {
		if t, ok := parseLogTime(tm[1]); ok {
			o.TS = t.UTC().Format(time.RFC3339)
		}
	}
	return o, true
}

func parseLogTime(s string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05.999999999Z0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// Action is what an observation means for the SLA.
type Action string

// Actions a policy can assign.
const (
	NodeDown Action = "node_down" // opens an incident on the node
	Degraded Action = "degraded"  // opens a degraded interval; needs an integration that closes it
	App      Action = "app"       // application-level; recorded, no SLA event
	Review   Action = "review"    // recorded and flagged for a human
	Observe  Action = "observe"   // recorded only; expected or informational
)

// Rule maps one code to an action.
type Rule struct {
	Action  Action `json:"action"`
	Summary string `json:"summary,omitempty"`
}

// Policy maps XID and SXID codes to actions.
type Policy struct {
	Version string          `json:"version"`
	Note    string          `json:"note,omitempty"`
	XID     map[string]Rule `json:"xid"`
	SXID    map[string]Rule `json:"sxid,omitempty"`
	AMD     map[string]Rule `json:"amd,omitempty"` // keyed by amdgpu class
	Default Rule            `json:"default"`
}

//go:embed default_policy.json
var defaultPolicyJSON []byte

// DefaultPolicyJSON returns the embedded starter policy.
func DefaultPolicyJSON() []byte { return append([]byte(nil), defaultPolicyJSON...) }

// DefaultPolicy returns the parsed starter policy.
func DefaultPolicy() (Policy, error) { return ParsePolicy(defaultPolicyJSON) }

// LoadPolicy reads a policy file.
func LoadPolicy(path string) (Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Policy{}, err
	}
	p, err := ParsePolicy(b)
	if err != nil {
		return Policy{}, fmt.Errorf("%s: %w", path, err)
	}
	return p, nil
}

// ParsePolicy decodes and validates a policy. Unknown fields are rejected.
func ParsePolicy(b []byte) (Policy, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return Policy{}, err
	}
	if p.Version == "" {
		return Policy{}, fmt.Errorf("policy version is required")
	}
	check := func(where string, r Rule) error {
		switch r.Action {
		case NodeDown, Degraded, App, Review, Observe:
			return nil
		}
		return fmt.Errorf("%s: unknown action %q", where, r.Action)
	}
	if err := check("default", p.Default); err != nil {
		return Policy{}, err
	}
	for code, r := range p.XID {
		if _, err := strconv.Atoi(code); err != nil {
			return Policy{}, fmt.Errorf("xid %q: code must be a number", code)
		}
		if err := check("xid "+code, r); err != nil {
			return Policy{}, err
		}
	}
	for code, r := range p.SXID {
		if _, err := strconv.Atoi(code); err != nil {
			return Policy{}, fmt.Errorf("sxid %q: code must be a number", code)
		}
		if err := check("sxid "+code, r); err != nil {
			return Policy{}, err
		}
	}
	for class, r := range p.AMD {
		if !knownAMDClass(class) {
			return Policy{}, fmt.Errorf("amd %q: unknown class (known: %s)", class, strings.Join(AMDClasses, ", "))
		}
		if err := check("amd "+class, r); err != nil {
			return Policy{}, err
		}
	}
	return p, nil
}

// Classify returns the rule for an observation, falling back to the default.
func (p Policy) Classify(o Observation) Rule {
	if o.Kind == "amdgpu" {
		if r, ok := p.AMD[o.Class]; ok {
			return r
		}
		return p.Default
	}
	table := p.XID
	if o.Kind == "sxid" {
		table = p.SXID
	}
	if r, ok := table[strconv.Itoa(o.Code)]; ok {
		return r
	}
	return p.Default
}
