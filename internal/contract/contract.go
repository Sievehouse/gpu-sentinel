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

	"github.com/Sievehouse/gpu-sentinel/internal/canonical"
	"github.com/Sievehouse/gpu-sentinel/internal/yamlite"
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
	// Sources, since 0.3, lists which event sources the contract accepts and
	// which party each belongs to. Contracts without it accept every event,
	// as before.
	Sources *Sources `json:"sources,omitempty"`
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
	// Unmonitored says what stretches without a heartbeat mean: "report"
	// (the default: listed in the statement, no effect on availability) or
	// "count-as-down" (countable downtime, subject to the same exclusions).
	Unmonitored string `json:"unmonitored,omitempty"`
	// Reconcile, since 0.3, says how the provider's and the tenant's records
	// combine into one outage clock. Required when sources.tenant is set.
	Reconcile *Reconcile `json:"reconcile,omitempty"`
}

// Reconcile decides, for every episode of downtime that either party
// recorded on a node, whether it counts and when it starts and ends. An
// episode is a stretch in which at least one party recorded the node as
// unavailable without interruption.
type Reconcile struct {
	// Count is "either" (an episode counts if either party recorded it),
	// "both" (only if both did), "provider" or "tenant" (only if that party
	// did).
	Count string `json:"count"`
	// Start is "earliest" (the first record of either party), "latest" (the
	// later of the two parties' first records), "provider" or "tenant" (that
	// party's first record; the other party's if it recorded nothing).
	Start string `json:"start"`
	// End is "latest", "earliest", "provider" or "tenant", the same way
	// round: "tenant" ends the episode when the tenant saw the node again.
	End string `json:"end"`
}

// Reconcile values.
const (
	SideProvider  = "provider"
	SideTenant    = "tenant"
	CountEither   = "either"
	CountBoth     = "both"
	ClockEarliest = "earliest"
	ClockLatest   = "latest"
)

// Sources is the contract's allow-list of event sources, by party.
type Sources struct {
	Provider *SourceSide `json:"provider,omitempty"`
	Tenant   *SourceSide `json:"tenant,omitempty"`
	// Unlisted is "reject" (the default: an event from any other source
	// stops the computation) or "ignore" (such events are skipped and
	// counted in the statement's warnings).
	Unlisted string `json:"unlisted,omitempty"`
}

// SourceSide lists the sources one party speaks through. An event that
// carries a signer role (from "events export --signers") belongs to the
// side whose roles include it; its self-declared source name is then not
// considered. Other events are matched by their source name. Roles are
// attested by the keyring and so are the stronger choice: a contract that
// lists only roles accepts only events from verified, signed logs.
type SourceSide struct {
	Roles   []string `json:"roles,omitempty"`
	Sources []string `json:"sources,omitempty"`
}

// Unlisted-source policies.
const (
	UnlistedReject = "reject"
	UnlistedIgnore = "ignore"
)

// Unmonitored-time policies.
const (
	UnmonitoredReport      = "report"
	UnmonitoredCountAsDown = "count-as-down"
)

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
	byRole  map[string]string // signer role -> side
	byName  map[string]string // source name -> side
}

// HasSources reports whether the contract restricts event sources (0.3).
func (s *Spec) HasSources() bool { return s.Contract.Sources != nil }

// Reconciles reports whether the contract combines two parties' records.
func (s *Spec) Reconciles() bool { return s.Contract.Measurement.Reconcile != nil }

// SideOf returns the party ("provider" or "tenant") an event belongs to
// under the contract's sources, or false if the source is not listed. An
// event with a signer role is matched by that role only; one without is
// matched by its source name. Without a sources block every event belongs
// to the provider side.
func (s *Spec) SideOf(signerRole, source string) (string, bool) {
	if s.Contract.Sources == nil {
		return SideProvider, true
	}
	if signerRole != "" {
		side, ok := s.byRole[signerRole]
		return side, ok
	}
	side, ok := s.byName[source]
	return side, ok
}

// IgnoresUnlisted reports whether events from unlisted sources are skipped
// rather than rejected.
func (s *Spec) IgnoresUnlisted() bool {
	return s.Contract.Sources != nil && s.Contract.Sources.Unlisted == UnlistedIgnore
}

// CountsUnmonitored reports whether time without heartbeats is downtime.
func (s *Spec) CountsUnmonitored() bool {
	return s.Contract.Measurement.Unmonitored == UnmonitoredCountAsDown
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
	switch c.Measurement.Unmonitored {
	case "", UnmonitoredReport, UnmonitoredCountAsDown:
	default:
		fail("measurement.unmonitored must be %q or %q", UnmonitoredReport, UnmonitoredCountAsDown)
	}
	parseSources(c, s, fail)
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

var sourceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,127}$`)
var roleSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// parseSources validates sources and measurement.reconcile and fills the
// role and name indexes.
func parseSources(c Contract, s *Spec, fail func(string, ...any)) {
	s.byRole, s.byName = map[string]string{}, map[string]string{}
	src, rec := c.Sources, c.Measurement.Reconcile
	if src == nil {
		if rec != nil {
			fail("measurement.reconcile needs sources.provider and sources.tenant")
		}
		return
	}
	switch src.Unlisted {
	case "", UnlistedReject, UnlistedIgnore:
	default:
		fail("sources.unlisted must be %q or %q", UnlistedReject, UnlistedIgnore)
	}
	if src.Provider == nil {
		fail("sources.provider is required when sources is set")
	}
	for _, side := range []struct {
		name string
		v    *SourceSide
	}{{SideProvider, src.Provider}, {SideTenant, src.Tenant}} {
		if side.v == nil {
			continue
		}
		if len(side.v.Roles) == 0 && len(side.v.Sources) == 0 {
			fail("sources.%s needs at least one role or source", side.name)
		}
		for _, r := range side.v.Roles {
			if !roleSlugRe.MatchString(r) {
				fail("sources.%s.roles: %q is not a keyring role (a lowercase slug)", side.name, r)
				continue
			}
			if other, dup := s.byRole[r]; dup {
				fail("role %q is listed for both sources.%s and sources.%s", r, other, side.name)
			}
			s.byRole[r] = side.name
		}
		for _, n := range side.v.Sources {
			if !sourceNameRe.MatchString(n) {
				fail("sources.%s.sources: %q is not a valid source name", side.name, n)
				continue
			}
			if other, dup := s.byName[n]; dup {
				fail("source %q is listed for both sources.%s and sources.%s", n, other, side.name)
			}
			s.byName[n] = side.name
		}
	}
	if src.Tenant != nil && rec == nil {
		fail("measurement.reconcile is required when sources.tenant is set: the contract must say how the two records combine")
	}
	if rec == nil {
		return
	}
	if src.Tenant == nil {
		fail("measurement.reconcile needs sources.tenant")
	}
	switch rec.Count {
	case CountEither, CountBoth, SideProvider, SideTenant:
	default:
		fail("measurement.reconcile.count must be either, both, provider or tenant")
	}
	for _, f := range []struct{ name, v string }{{"start", rec.Start}, {"end", rec.End}} {
		switch f.v {
		case ClockEarliest, ClockLatest, SideProvider, SideTenant:
		default:
			fail("measurement.reconcile.%s must be earliest, latest, provider or tenant", f.name)
		}
	}
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
