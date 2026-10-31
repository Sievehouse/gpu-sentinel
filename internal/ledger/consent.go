package ledger

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/canonical"
	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
)

// ConsentKind identifies a financier feed consent.
const ConsentKind = "FeedConsent"

const consentDomain = "capacity-ledger/feed-consent/v1\n"

// Consent decisions.
const (
	Grant  = "grant"
	Revoke = "revoke"
)

// Consent is the provider's or the customer's signed permission (or its
// withdrawal) for one financier to read the contract's feed: the sealed
// statements, their signatures and the log checkpoints, never the raw
// logs. A financier sees the feed only while both parties' latest consents
// for it are grants.
type Consent struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Contract   string `json:"contract"`
	Financier  string `json:"financier"`
	Party      string `json:"party"`
	Decision   string `json:"decision"`
	Note       string `json:"note,omitempty"`
	SignedAt   string `json:"signedAt"`
	KeyID      string `json:"keyId"`
	Sig        string `json:"sig"`
}

func (c *Consent) digest() ([]byte, error) {
	cp := *c
	cp.Sig = ""
	b, err := canonical.Marshal(cp)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(consentDomain))
	h.Write(b)
	return h.Sum(nil), nil
}

// SignConsent creates a consent signed with priv. party is the signer's
// role: provider or customer.
func SignConsent(contractID, financier, party, decision, note string, priv ed25519.PrivateKey, now time.Time) (*Consent, error) {
	if party != RoleProvider && party != RoleCustomer {
		return nil, errors.New("only the provider and the customer give consent")
	}
	if decision != Grant && decision != Revoke {
		return nil, fmt.Errorf("decision must be %q or %q", Grant, Revoke)
	}
	if !nameRe.MatchString(financier) {
		return nil, fmt.Errorf("financier %q must be the financier's party name on the ledger", financier)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	c := &Consent{
		APIVersion: APIVersion, Kind: ConsentKind, Contract: contractID, Financier: financier, Party: party,
		Decision: decision, Note: note, SignedAt: now.UTC().Format(time.RFC3339), KeyID: evidence.KeyID(pub),
	}
	d, err := c.digest()
	if err != nil {
		return nil, err
	}
	c.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, d))
	return c, nil
}

// Signer looks up a key valid at t and returns it with its keyring role.
type Signer func(keyID string, t time.Time) (ed25519.PublicKey, string, error)

// Check verifies the consent's form and signature; the signing key's
// keyring role must be the consenting party.
func (c *Consent) Check(contractID string, lookup Signer) error {
	if c.APIVersion != APIVersion || c.Kind != ConsentKind {
		return fmt.Errorf("not a %s %s document", APIVersion, ConsentKind)
	}
	if c.Contract != contractID {
		return fmt.Errorf("consent is for contract %q, not %q", c.Contract, contractID)
	}
	if c.Decision != Grant && c.Decision != Revoke {
		return fmt.Errorf("unknown decision %q", c.Decision)
	}
	if c.Party != RoleProvider && c.Party != RoleCustomer {
		return fmt.Errorf("party %q cannot give consent", c.Party)
	}
	t, err := time.Parse(time.RFC3339, c.SignedAt)
	if err != nil {
		return fmt.Errorf("signedAt: %w", err)
	}
	pub, role, err := lookup(c.KeyID, t)
	if err != nil {
		return err
	}
	if role != c.Party {
		return fmt.Errorf("consent given as %q by key %s, whose keyring role is %q", c.Party, c.KeyID, role)
	}
	d, err := c.digest()
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(c.Sig)
	if err != nil || !ed25519.Verify(pub, d, raw) {
		return errors.New("invalid consent signature")
	}
	return nil
}

// Time returns when the consent was signed.
func (c *Consent) Time() time.Time {
	t, _ := time.Parse(time.RFC3339, c.SignedAt)
	return t
}

// DecodeConsent parses a consent document, rejecting unknown fields.
func DecodeConsent(b []byte) (*Consent, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Consent
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}

// FeedOpen reports whether financier may read the feed: both the provider's
// and the customer's latest valid consents for it must be grants. It also
// returns, for a closed feed, which parties have not granted.
func FeedOpen(consents []*Consent, financier string) (bool, []string) {
	latest := map[string]*Consent{}
	for _, c := range consents {
		if c.Financier != financier {
			continue
		}
		// On equal times the later one in the list (the later upload) wins.
		if cur, ok := latest[c.Party]; !ok || !c.Time().Before(cur.Time()) {
			latest[c.Party] = c
		}
	}
	var missing []string
	for _, party := range []string{RoleProvider, RoleCustomer} {
		if c, ok := latest[party]; !ok || c.Decision != Grant {
			missing = append(missing, party)
		}
	}
	return len(missing) == 0, missing
}
