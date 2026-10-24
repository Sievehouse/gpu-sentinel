// Package keyring records which signing keys a verifier trusts, who holds
// them, what role they play and which nodes they may speak for.
//
// A key announcement in an evidence log proves only that someone holds the
// private key. Trust is a separate, explicit decision: an operator enrols the
// key here with an owner and a role, and optionally binds it to the nodes it
// may report on, so a key stolen from one node cannot report outages (or
// recoveries) for another.
package keyring

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
)

// APIVersion and Kind identify a keyring document.
const (
	APIVersion = "capacity-ledger/v1alpha1"
	Kind       = "Keyring"
)

// Suggested roles. Any lowercase slug is accepted.
const (
	RoleProviderAgent       = "provider-agent"
	RoleProviderIntegration = "provider-integration"
	RoleTenantProbe         = "tenant-probe"
	RoleHarness             = "harness"
	RoleAuditor             = "auditor"
)

var roleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Keyring is the on-disk JSON form.
type Keyring struct {
	APIVersion string  `json:"apiVersion"`
	Kind       string  `json:"kind"`
	Keys       []Entry `json:"keys"`
}

// Entry is one trusted key.
type Entry struct {
	KeyID     string   `json:"keyId"`
	PublicKey string   `json:"publicKey"`
	Owner     string   `json:"owner"`
	Role      string   `json:"role"`
	Nodes     []string `json:"nodes,omitempty"`
	NotBefore string   `json:"notBefore,omitempty"`
	NotAfter  string   `json:"notAfter,omitempty"`
	Note      string   `json:"note,omitempty"`

	pub       ed25519.PublicKey
	notBefore time.Time
	notAfter  time.Time
	nodes     map[string]bool
}

// New returns an empty keyring.
func New() *Keyring { return &Keyring{APIVersion: APIVersion, Kind: Kind, Keys: []Entry{}} }

// Load reads a keyring file. Unknown fields are rejected.
func Load(path string) (*Keyring, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var k Keyring
	if err := dec.Decode(&k); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if k.APIVersion != APIVersion || k.Kind != Kind {
		return nil, fmt.Errorf("%s: not a %s %s document", path, APIVersion, Kind)
	}
	entries := k.Keys
	k.Keys = []Entry{}
	for _, e := range entries {
		if err := k.Add(e); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	return &k, nil
}

// LoadOrNew loads path, or returns an empty keyring if it does not exist.
func LoadOrNew(path string) (*Keyring, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	return Load(path)
}

// Save writes the keyring atomically, sorted by owner, role and key id.
func (k *Keyring) Save(path string) error {
	sort.SliceStable(k.Keys, func(i, j int) bool {
		a, b := k.Keys[i], k.Keys[j]
		if a.Owner != b.Owner {
			return a.Owner < b.Owner
		}
		if a.Role != b.Role {
			return a.Role < b.Role
		}
		return a.KeyID < b.KeyID
	})
	b, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".keyring-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Add validates e and adds it. A key may appear only once.
func (k *Keyring) Add(e Entry) error {
	pub, err := evidence.ParsePublicKeyPEM(e.PublicKey)
	if err != nil {
		return fmt.Errorf("key %s: %w", e.KeyID, err)
	}
	id := evidence.KeyID(pub)
	if e.KeyID == "" {
		e.KeyID = id
	}
	if e.KeyID != id {
		return fmt.Errorf("key id %s does not match its public key (%s)", e.KeyID, id)
	}
	if e.Owner == "" {
		return fmt.Errorf("key %s: owner is required", id)
	}
	if !roleRe.MatchString(e.Role) {
		return fmt.Errorf("key %s: role %q must be a lowercase slug such as %q", id, e.Role, RoleProviderAgent)
	}
	if e.NotBefore != "" {
		if e.notBefore, err = time.Parse(time.RFC3339, e.NotBefore); err != nil {
			return fmt.Errorf("key %s: notBefore: %w", id, err)
		}
	}
	if e.NotAfter != "" {
		if e.notAfter, err = time.Parse(time.RFC3339, e.NotAfter); err != nil {
			return fmt.Errorf("key %s: notAfter: %w", id, err)
		}
		if !e.notBefore.IsZero() && !e.notAfter.After(e.notBefore) {
			return fmt.Errorf("key %s: notAfter must be after notBefore", id)
		}
	}
	e.nodes = map[string]bool{}
	for _, n := range e.Nodes {
		if n == "" {
			return fmt.Errorf("key %s: empty node name", id)
		}
		e.nodes[n] = true
	}
	for _, existing := range k.Keys {
		if existing.KeyID == id {
			return fmt.Errorf("key %s is already in the keyring (owner %s, role %s)", id, existing.Owner, existing.Role)
		}
	}
	e.pub = pub
	k.Keys = append(k.Keys, e)
	return nil
}

// Lookup returns the entry for a key id.
func (k *Keyring) Lookup(id string) (*Entry, bool) {
	for i := range k.Keys {
		if k.Keys[i].KeyID == id {
			return &k.Keys[i], true
		}
	}
	return nil, false
}

// Public returns the entry's public key.
func (e *Entry) Public() ed25519.PublicKey { return e.pub }

// KeyAt returns the entry for id if the key is enrolled and valid at t.
func (k *Keyring) KeyAt(id string, t time.Time) (*Entry, error) {
	e, ok := k.Lookup(id)
	if !ok {
		return nil, fmt.Errorf("key %s is not in the keyring", id)
	}
	if err := e.CheckTime(t); err != nil {
		return nil, err
	}
	return e, nil
}

// PublicKeys returns every trusted key indexed by id, for evidence.Verify.
func (k *Keyring) PublicKeys() map[string]ed25519.PublicKey {
	out := make(map[string]ed25519.PublicKey, len(k.Keys))
	for _, e := range k.Keys {
		out[e.KeyID] = e.pub
	}
	return out
}

// CheckTime reports whether the key was valid at t (the writer's clock).
func (e *Entry) CheckTime(t time.Time) error {
	if !e.notBefore.IsZero() && t.Before(e.notBefore) {
		return fmt.Errorf("key %s (%s) was used at %s, before its notBefore %s", e.KeyID, e.Owner, t.UTC().Format(time.RFC3339), e.NotBefore)
	}
	if !e.notAfter.IsZero() && !t.Before(e.notAfter) {
		return fmt.Errorf("key %s (%s) was used at %s, after its notAfter %s", e.KeyID, e.Owner, t.UTC().Format(time.RFC3339), e.NotAfter)
	}
	return nil
}

// AllowsNode reports whether the key may speak for node. A key bound to no
// nodes may speak for any node.
func (e *Entry) AllowsNode(node string) bool { return len(e.nodes) == 0 || e.nodes[node] }

// EntryCheck returns an evidence.EntryCheck that enforces the keyring's
// rules on every correctly signed entry: the key's validity window at the
// entry's time and, for entries about one node (events, heartbeats,
// observations and key announcements), the key's node bindings.
func (k *Keyring) EntryCheck() evidence.EntryCheck {
	return func(en evidence.Entry) error {
		e, ok := k.Lookup(en.KeyID)
		if !ok {
			return fmt.Errorf("key %s is not in the keyring", en.KeyID)
		}
		t, err := time.Parse(time.RFC3339Nano, en.TS)
		if err != nil {
			return fmt.Errorf("bad entry timestamp %q", en.TS)
		}
		if err := e.CheckTime(t); err != nil {
			return err
		}
		node, err := PayloadNode(en)
		if err != nil {
			return err
		}
		if node != "" && !e.AllowsNode(node) {
			return fmt.Errorf("key %s (%s, %s) may speak only for %s but signed a %s entry for node %s", e.KeyID, e.Owner, e.Role, strings.Join(e.Nodes, ", "), en.Kind, node)
		}
		return nil
	}
}

// PayloadNode returns the node an entry is about, or "" for entries that are
// not about a single node (statements, bundles, other kinds).
func PayloadNode(en evidence.Entry) (string, error) {
	switch en.Kind {
	case evidence.KindEvent, evidence.KindHeartbeat, evidence.KindObservation, evidence.KindKey:
	default:
		return "", nil
	}
	var p struct {
		Node string `json:"node"`
	}
	if err := en.DecodePayload(&p); err != nil {
		return "", fmt.Errorf("%s entry: %w", en.Kind, err)
	}
	return p.Node, nil
}

// CheckNode returns an error if the key that signed en may not speak for node.
func (k *Keyring) CheckNode(en evidence.Entry, node string) error {
	e, ok := k.Lookup(en.KeyID)
	if !ok {
		return fmt.Errorf("key %s is not in the keyring", en.KeyID)
	}
	if !e.AllowsNode(node) {
		return fmt.Errorf("key %s (%s, %s) is bound to %v but signed a %s entry for node %s", e.KeyID, e.Owner, e.Role, e.Nodes, en.Kind, node)
	}
	return nil
}

// FromPublicKeyFiles builds an ad-hoc keyring from PEM files. Such keys are
// trusted for every node, as --pub has always been.
func FromPublicKeyFiles(paths []string, owner string) (*Keyring, error) {
	k := New()
	for _, p := range paths {
		pub, err := evidence.LoadPublicKey(p)
		if err != nil {
			return nil, err
		}
		pemText, err := evidence.PublicKeyPEM(pub)
		if err != nil {
			return nil, err
		}
		if err := k.Add(Entry{PublicKey: pemText, Owner: owner, Role: "cli", Note: "from " + p}); err != nil {
			if _, dup := k.Lookup(evidence.KeyID(pub)); dup {
				continue
			}
			return nil, err
		}
	}
	return k, nil
}

// Merge adds every key of o that k does not already hold.
func (k *Keyring) Merge(o *Keyring) error {
	for _, e := range o.Keys {
		if _, dup := k.Lookup(e.KeyID); dup {
			continue
		}
		if err := k.Add(e); err != nil {
			return err
		}
	}
	return nil
}

// Enrollment describes a key found in a log that an operator may enrol.
type Enrollment struct {
	Announcement evidence.KeyAnnouncement
	Seq          int64
}

// FindAnnouncements returns every valid key announcement in the given log
// entries. Invalid announcements are returned as errors.
func FindAnnouncements(entries []evidence.Entry) ([]Enrollment, []error) {
	var out []Enrollment
	var errs []error
	for _, en := range entries {
		if en.Kind != evidence.KindKey {
			continue
		}
		a, _, err := evidence.CheckAnnouncement(en)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, Enrollment{Announcement: a, Seq: en.Seq})
	}
	return out, errs
}
