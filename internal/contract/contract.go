// Package contract loads and validates the machine-readable SLA terms of a
// GPU capacity contract.
//
// The spec is deliberately small. It captures what the engine needs to turn
// events into availability and credits: which nodes are covered, how long an
// outage must last to count, which downtime is excluded, how racks fail as a
// unit, and the credit tiers. Everything else in a real contract stays in
// the contract.
package contract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Sievehouse/capacity-ledger/internal/canonical"
	"github.com/Sievehouse/capacity-ledger/internal/yamlite"
)

// APIVersion and Kind identify a contract spec document.
const (
	APIVersion = "capacity-ledger/v1alpha1"
	Kind       = "ContractSLA"
)

// Contract is the on-disk JSON form of a contract SLA spec.
type Contract struct {
	APIVersion  string      `json:"apiVersion"`
	Kind        string      `json:"kind"`
	Metadata    Metadata    `json:"metadata"`
	Cluster     Cluster     `json:"cluster"`
	Measurement Measurement `json:"measurement"`
	Exclusions  Exclusions  `json:"exclusions"`
	Credits     Credits     `json:"credits"`
}

// Metadata identifies the contract and its version.
type Metadata struct {
	ID          string `json:"id"`
	Version     int    `json:"version"`
	Provider    string `json:"provider,omitempty"`
	Customer    string `json:"customer,omitempty"`
	Description string `json:"description,omitempty"`
}

// Cluster lists the covered nodes and, for rack-scale systems, the racks.
type Cluster struct {
	GPUsPerNode int      `json:"gpusPerNode"`
	Nodes       []string `json:"nodes"`
	Racks       []Rack   `json:"racks,omitempty"`
}

// Rack groups nodes that share a scale-up domain, such as an NVL72 rack.
type Rack struct {
	Name  string   `json:"name"`
	Nodes []string `json:"nodes"`
}

// Measurement controls how raw unavailability becomes countable downtime.
type Measurement struct {
	// MinIncident is a Go duration such as "5m". Countable outages shorter
	// than this are ignored. Empty means every outage counts.
	MinIncident string `json:"minIncident,omitempty"`
	// DegradedCountsAsDown makes degraded intervals countable downtime.
	DegradedCountsAsDown bool `json:"degradedCountsAsDown"`
	// RackRule, when set, marks a whole rack down while at least
	// NodesDownThreshold of its nodes are down.
	RackRule *RackRule `json:"rackRule,omitempty"`
}

// RackRule marks every node of a rack down while enough of its nodes are down.
type RackRule struct {
	NodesDownThreshold int `json:"nodesDownThreshold"`
}

// Exclusions lists downtime that does not count against the provider.
type Exclusions struct {
	// MaintenanceNotice is the minimum advance notice, as a Go duration, for
	// maintenance to be excluded. Empty means maintenance is never excluded.
	MaintenanceNotice string `json:"maintenanceNotice,omitempty"`
	// MaxExcludedPerNode caps excluded time per node per period. Empty means
	// no cap. Excluded time beyond the cap counts as downtime.
	MaxExcludedPerNode string `json:"maxExcludedPerNode,omitempty"`
	// CustomerCaused excludes downtime whose every cause starts with "customer".
	CustomerCaused bool `json:"customerCaused"`
}

// Credits defines service credits by availability tier.
type Credits struct {
	MonthlyFee string `json:"monthlyFee,omitempty"`
	Currency   string `json:"currency,omitempty"`
	// CapPercent caps the credit; 0 means no cap.
	CapPercent int    `json:"capPercent"`
	Tiers      []Tier `json:"tiers"`
}

// Tier grants CreditPercent when availability is strictly below
// AvailabilityBelow (a decimal percentage such as "99.9").
type Tier struct {
	AvailabilityBelow string `json:"availabilityBelow"`
	CreditPercent     int    `json:"creditPercent"`
}

// ParsedTier is a validated tier with an exact threshold.
type ParsedTier struct {
	Below     *big.Rat
	BelowText string
	Percent   int
}

// Spec is a validated contract with parsed values.
type Spec struct {
	Contract              Contract
	MinIncident           int64 // seconds
	MaintenanceExcludable bool
	MaintenanceNotice     int64 // seconds
	ExclusionCap          int64 // seconds; -1 means no cap
	Fee                   *big.Rat
	Tiers                 []ParsedTier

	nodeSet map[string]struct{}
	rackOf  map[string]string
}

// HasNode reports whether the contract covers node.
func (s *Spec) HasNode(node string) bool { _, ok := s.nodeSet[node]; return ok }

// RackOf returns the rack a node belongs to, if any.
func (s *Spec) RackOf(node string) (string, bool) { r, ok := s.rackOf[node]; return r, ok }

// SortedRacks returns the racks ordered by name.
func (s *Spec) SortedRacks() []Rack {
	racks := append([]Rack(nil), s.Contract.Cluster.Racks...)
	sort.Slice(racks, func(i, j int) bool { return racks[i].Name < racks[j].Name })
	return racks
}

// SortedNodes returns the covered nodes ordered by name.
func (s *Spec) SortedNodes() []string {
	nodes := append([]string(nil), s.Contract.Cluster.Nodes...)
	sort.Strings(nodes)
	return nodes
}

// Digest returns the SHA-256 of the canonical encoding of the contract.
func (s *Spec) Digest() (string, error) { return canonical.SHA256Hex(s.Contract) }

// Load reads and validates a contract spec from a JSON or YAML file. Files
// ending in .yaml or .yml are read as YAML; anything else is sniffed.
func Load(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	format := ""
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		format = "yaml"
	case ".json":
		format = "json"
	}
	spec, err := Decode(b, format)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return spec, nil
}

// Read decodes and validates a contract spec in JSON or YAML.
func Read(r io.Reader) (*Spec, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return Decode(b, "")
}

// Decode parses a contract spec. format is "json", "yaml" or "" to sniff:
// a document whose first non-blank character is '{' is JSON. Unknown fields
// are rejected in both formats, and both produce the same Contract value,
// so a YAML spec and its JSON twin have the same digest.
func Decode(b []byte, format string) (*Spec, error) {
	if format == "" {
		trimmed := bytes.TrimLeft(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), " \t\r\n")
		format = "yaml"
		if len(trimmed) > 0 && trimmed[0] == '{' {
			format = "json"
		}
	}
	var c Contract
	switch format {
	case "json":
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, err
		}
	case "yaml":
		if err := yamlite.Unmarshal(b, &c); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unknown contract format %q", format)
	}
	return Parse(c)
}

var decimalRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// Parse validates a decoded contract and returns a Spec. All problems are
// reported together.
func Parse(c Contract) (*Spec, error) {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.APIVersion != APIVersion {
		fail("apiVersion must be %q, got %q", APIVersion, c.APIVersion)
	}
	if c.Kind != Kind {
		fail("kind must be %q, got %q", Kind, c.Kind)
	}
	if strings.TrimSpace(c.Metadata.ID) == "" {
		fail("metadata.id is required")
	}
	if c.Metadata.Version < 1 {
		fail("metadata.version must be at least 1")
	}
	if c.Cluster.GPUsPerNode < 1 {
		fail("cluster.gpusPerNode must be at least 1")
	}
	if len(c.Cluster.Nodes) == 0 {
		fail("cluster.nodes must not be empty")
	}

	s := &Spec{Contract: c, nodeSet: map[string]struct{}{}, rackOf: map[string]string{}, ExclusionCap: -1}
	for _, n := range c.Cluster.Nodes {
		if strings.TrimSpace(n) == "" {
			fail("cluster.nodes contains an empty name")
			continue
		}
		if _, dup := s.nodeSet[n]; dup {
			fail("cluster.nodes lists %q twice", n)
		}
		s.nodeSet[n] = struct{}{}
	}
	rackNames := map[string]bool{}
	for _, r := range c.Cluster.Racks {
		if r.Name == "" {
			fail("every rack needs a name")
		}
		if rackNames[r.Name] {
			fail("rack %q defined twice", r.Name)
		}
		rackNames[r.Name] = true
		for _, n := range r.Nodes {
			if _, ok := s.nodeSet[n]; !ok {
				fail("rack %q lists %q, which is not in cluster.nodes", r.Name, n)
			}
			if other, dup := s.rackOf[n]; dup {
				fail("node %q is in racks %q and %q", n, other, r.Name)
			}
			s.rackOf[n] = r.Name
		}
	}

	var err error
	if s.MinIncident, err = seconds(c.Measurement.MinIncident); err != nil {
		fail("measurement.minIncident: %v", err)
	}
	if rr := c.Measurement.RackRule; rr != nil {
		if len(c.Cluster.Racks) == 0 {
			fail("measurement.rackRule needs cluster.racks")
		}
		if rr.NodesDownThreshold < 1 {
			fail("measurement.rackRule.nodesDownThreshold must be at least 1")
		}
	}
	if c.Exclusions.MaintenanceNotice != "" {
		s.MaintenanceExcludable = true
		if s.MaintenanceNotice, err = seconds(c.Exclusions.MaintenanceNotice); err != nil {
			fail("exclusions.maintenanceNotice: %v", err)
		}
	}
	if c.Exclusions.MaxExcludedPerNode != "" {
		if s.ExclusionCap, err = seconds(c.Exclusions.MaxExcludedPerNode); err != nil {
			fail("exclusions.maxExcludedPerNode: %v", err)
		}
	}

	if c.Credits.CapPercent < 0 || c.Credits.CapPercent > 100 {
		fail("credits.capPercent must be between 0 and 100")
	}
	if c.Credits.MonthlyFee != "" {
		if !decimalRe.MatchString(c.Credits.MonthlyFee) {
			fail("credits.monthlyFee must be a plain decimal such as \"300000.00\"")
		} else {
			s.Fee, _ = new(big.Rat).SetString(c.Credits.MonthlyFee)
		}
	}
	hundred := big.NewRat(100, 1)
	for i, t := range c.Credits.Tiers {
		if !decimalRe.MatchString(t.AvailabilityBelow) {
			fail("credits.tiers[%d].availabilityBelow must be a plain decimal percentage", i)
			continue
		}
		below, _ := new(big.Rat).SetString(t.AvailabilityBelow)
		if below.Sign() <= 0 || below.Cmp(hundred) > 0 {
			fail("credits.tiers[%d].availabilityBelow must be in (0, 100]", i)
		}
		if t.CreditPercent < 0 || t.CreditPercent > 100 {
			fail("credits.tiers[%d].creditPercent must be between 0 and 100", i)
		}
		s.Tiers = append(s.Tiers, ParsedTier{Below: below, BelowText: t.AvailabilityBelow, Percent: t.CreditPercent})
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return s, nil
}

// seconds parses a Go duration into whole seconds. Empty means zero.
func seconds(d string) (int64, error) {
	if strings.TrimSpace(d) == "" {
		return 0, nil
	}
	v, err := time.ParseDuration(d)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, errors.New("must not be negative")
	}
	return int64(v / time.Second), nil
}
