// Package ledger is the hosted ledger service: one small HTTP server per
// contract that both parties push their signed evidence logs to, that
// witnesses those logs with signed checkpoints, keeps the sealed monthly
// statements with every party's signature, and gives financiers a read-only
// feed once both the provider and the customer have consented.
//
// The server verifies everything it accepts against a keyring, but it is
// not trusted to compute anything: statements are computed and checked by
// the parties with the same capledger commands as without a server, and
// every document the server hands out can be verified offline.
package ledger

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Configuration document identifiers.
const (
	APIVersion = "capacity-ledger/v1alpha1"
	ConfigKind = "LedgerConfig"
)

// Party roles on a ledger.
const (
	RoleProvider  = "provider"
	RoleCustomer  = "customer"
	RoleFinancier = "financier"
	RoleAuditor   = "auditor"
)

// Config lists the contract a ledger serves and the parties that may use it.
type Config struct {
	APIVersion string  `json:"apiVersion"`
	Kind       string  `json:"kind"`
	Contract   string  `json:"contract"`
	Parties    []Party `json:"parties"`
}

// Party is one organisation with access to the ledger.
type Party struct {
	// Name identifies the party, e.g. "example-neocloud".
	Name string `json:"name"`
	// Role is provider, customer, financier or auditor.
	Role string `json:"role"`
	// TokenSHA256 is the hex SHA-256 of the party's bearer token; the
	// server never stores the token itself (see "capledger ledger token").
	TokenSHA256 string `json:"tokenSha256"`
	// WriteRoles are the keyring roles of the keys whose logs this party may
	// push, e.g. provider-agent for the provider or tenant-probe for the
	// customer. Financiers and auditors push nothing.
	WriteRoles []string `json:"writeRoles,omitempty"`
	Note       string   `json:"note,omitempty"`
}

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	hexRe  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// LoadConfig reads and validates a configuration file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate checks the configuration. A ledger serves exactly one provider
// and one customer, and any number of financiers and auditors.
func (c *Config) Validate() error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.APIVersion != APIVersion || c.Kind != ConfigKind {
		fail("not a %s %s document", APIVersion, ConfigKind)
	}
	if strings.TrimSpace(c.Contract) == "" {
		fail("contract (the contract id this ledger serves) is required")
	}
	names, tokens, count := map[string]bool{}, map[string]bool{}, map[string]int{}
	for i, p := range c.Parties {
		if !nameRe.MatchString(p.Name) {
			fail("parties[%d]: name %q must be a lowercase slug", i, p.Name)
		}
		if names[p.Name] {
			fail("party %q is listed twice", p.Name)
		}
		names[p.Name] = true
		switch p.Role {
		case RoleProvider, RoleCustomer:
		case RoleFinancier, RoleAuditor:
			if len(p.WriteRoles) > 0 {
				fail("party %q: a %s pushes no logs, so writeRoles must be empty", p.Name, p.Role)
			}
		default:
			fail("party %q: role must be provider, customer, financier or auditor", p.Name)
		}
		count[p.Role]++
		if !hexRe.MatchString(p.TokenSHA256) {
			fail("party %q: tokenSha256 must be 64 lowercase hex characters (capledger ledger token prints one)", p.Name)
		}
		if tokens[p.TokenSHA256] {
			fail("party %q shares a token with another party", p.Name)
		}
		tokens[p.TokenSHA256] = true
		for _, r := range p.WriteRoles {
			if !nameRe.MatchString(r) {
				fail("party %q: writeRoles: %q is not a keyring role", p.Name, r)
			}
		}
	}
	if count[RoleProvider] != 1 || count[RoleCustomer] != 1 {
		fail("a ledger needs exactly one provider and one customer party (got %d and %d)", count[RoleProvider], count[RoleCustomer])
	}
	return errors.Join(errs...)
}

// ByToken returns the party holding token, comparing hashes in constant time.
func (c *Config) ByToken(token string) (*Party, bool) {
	if token == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(token))
	got := []byte(hex.EncodeToString(sum[:]))
	var found *Party
	for i := range c.Parties {
		if subtle.ConstantTimeCompare(got, []byte(c.Parties[i].TokenSHA256)) == 1 {
			found = &c.Parties[i]
		}
	}
	return found, found != nil
}

// ByRole returns the first party with role (the provider or the customer).
func (c *Config) ByRole(role string) (*Party, bool) {
	for i := range c.Parties {
		if c.Parties[i].Role == role {
			return &c.Parties[i], true
		}
	}
	return nil, false
}

// ByName returns the party called name.
func (c *Config) ByName(name string) (*Party, bool) {
	for i := range c.Parties {
		if c.Parties[i].Name == name {
			return &c.Parties[i], true
		}
	}
	return nil, false
}

// MayWrite reports whether the party may push logs signed by keys of role.
func (p *Party) MayWrite(role string) bool {
	for _, r := range p.WriteRoles {
		if r == role {
			return true
		}
	}
	return false
}

// TokenHash returns the hex SHA-256 of a token, for the configuration.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
