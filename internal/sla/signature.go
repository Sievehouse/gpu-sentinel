package sla

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/canonical"
	"github.com/Sievehouse/gpu-sentinel/internal/contract"
)

// SignatureKind identifies a statement signature document.
const SignatureKind = "StatementSignature"

// signatureDomain separates statement signatures from every other signed
// object, so a signature can never be replayed as something else.
const signatureDomain = "capacity-ledger/statement-signature/v1\n"

// Decisions a party can sign.
const (
	DecisionAccept  = "accept"
	DecisionDispute = "dispute"
)

// Parties that usually sign statements. Any keyring role slug is accepted.
const (
	PartyProvider  = "provider"
	PartyCustomer  = "customer"
	PartyFinancier = "financier"
)

var partyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Signature is one party's signed decision about one sealed statement. The
// party is the role of the signing key in the verifier's keyring, so a
// verifier decides who may sign as provider or customer, not the signer.
type Signature struct {
	APIVersion      string      `json:"apiVersion"`
	Kind            string      `json:"kind"`
	StatementSHA256 string      `json:"statementSha256"`
	Contract        ContractRef `json:"contract"`
	Period          PeriodRef   `json:"period"`
	Party           string      `json:"party"`
	Decision        string      `json:"decision"`
	Note            string      `json:"note,omitempty"`
	SignedAt        string      `json:"signedAt"`
	KeyID           string      `json:"keyId"`
	Sig             string      `json:"sig"`
}

// digest returns the SHA-256 that the signature covers: the domain and the
// canonical document with the sig field empty.
func (sg *Signature) digest() ([]byte, error) {
	cp := *sg
	cp.Sig = ""
	b, err := canonical.Marshal(cp)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(signatureDomain))
	h.Write(b)
	return h.Sum(nil), nil
}

// Sign records party's decision about st, signed with priv at now.
func Sign(st *Statement, party, decision, note string, priv ed25519.PrivateKey, now time.Time) (*Signature, error) {
	if err := st.CheckSeal(); err != nil {
		return nil, err
	}
	if !partyRe.MatchString(party) {
		return nil, fmt.Errorf("party %q must be a lowercase role such as %q or %q", party, PartyProvider, PartyCustomer)
	}
	if decision != DecisionAccept && decision != DecisionDispute {
		return nil, fmt.Errorf("decision must be %q or %q", DecisionAccept, DecisionDispute)
	}
	if decision == DecisionDispute && note == "" {
		return nil, errors.New("a dispute needs a note saying what is disputed")
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	sg := &Signature{
		APIVersion: contract.APIVersion, Kind: SignatureKind, StatementSHA256: st.StatementSHA256,
		Contract: st.Contract, Period: st.Period, Party: party, Decision: decision, Note: note,
		SignedAt: now.UTC().Format(time.RFC3339), KeyID: keyID(pub),
	}
	d, err := sg.digest()
	if err != nil {
		return nil, err
	}
	sg.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, d))
	return sg, nil
}

// keyID matches evidence.KeyID without importing the evidence package.
func keyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return fmt.Sprintf("%x", sum[:8])
}

// SignerLookup returns the public key and keyring role of a key that was
// valid at t, or an error if the key is unknown or was not valid then.
type SignerLookup func(keyID string, t time.Time) (pub ed25519.PublicKey, role string, err error)

// Check verifies one signature against the statement it claims to sign.
func (sg *Signature) Check(st *Statement, lookup SignerLookup) error {
	if sg.APIVersion != contract.APIVersion || sg.Kind != SignatureKind {
		return fmt.Errorf("not a %s %s document", contract.APIVersion, SignatureKind)
	}
	if sg.StatementSHA256 != st.StatementSHA256 {
		return fmt.Errorf("signs statement %s, not %s", short(sg.StatementSHA256), short(st.StatementSHA256))
	}
	if sg.Contract != st.Contract || sg.Period != st.Period {
		return errors.New("contract or period does not match the statement")
	}
	if sg.Decision != DecisionAccept && sg.Decision != DecisionDispute {
		return fmt.Errorf("unknown decision %q", sg.Decision)
	}
	t, err := time.Parse(time.RFC3339, sg.SignedAt)
	if err != nil {
		return fmt.Errorf("signedAt: %w", err)
	}
	pub, role, err := lookup(sg.KeyID, t)
	if err != nil {
		return err
	}
	if role != sg.Party {
		return fmt.Errorf("signed as %q by key %s, whose keyring role is %q", sg.Party, sg.KeyID, role)
	}
	d, err := sg.digest()
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(sg.Sig)
	if err != nil || !ed25519.Verify(pub, d, raw) {
		return errors.New("invalid signature")
	}
	return nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

// ReadSignature loads a signature file. Unknown fields are rejected.
func ReadSignature(path string) (*Signature, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	sg, err := DecodeSignature(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return sg, nil
}

// DecodeSignature parses a signature document, rejecting unknown fields.
func DecodeSignature(b []byte) (*Signature, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var sg Signature
	if err := dec.Decode(&sg); err != nil {
		return nil, err
	}
	return &sg, nil
}

// SignatureResult is the outcome of checking one signature.
type SignatureResult struct {
	Source   string `json:"source,omitempty"`
	Party    string `json:"party"`
	Decision string `json:"decision"`
	KeyID    string `json:"keyId"`
	SignedAt string `json:"signedAt"`
	Note     string `json:"note,omitempty"`
	Valid    bool   `json:"valid"`
	Problem  string `json:"problem,omitempty"`
}

// CoSignReport says whether every required party accepted a statement.
type CoSignReport struct {
	StatementSHA256 string            `json:"statementSha256"`
	Signatures      []SignatureResult `json:"signatures"`
	Required        []string          `json:"required"`
	AcceptedBy      []string          `json:"acceptedBy"`
	DisputedBy      []string          `json:"disputedBy"`
	Missing         []string          `json:"missing"`
	OK              bool              `json:"ok"`
}

// CheckCoSigned verifies every signature and reports whether each required
// party has a valid "accept" and none has a valid "dispute". When a party
// signed more than once, its latest valid signature counts. sources labels
// each signature (for example its file name) in the report.
func CheckCoSigned(st *Statement, sigs []*Signature, sources []string, lookup SignerLookup, required []string) CoSignReport {
	rep := CoSignReport{
		StatementSHA256: st.StatementSHA256, Signatures: []SignatureResult{}, Required: append([]string{}, required...),
		AcceptedBy: []string{}, DisputedBy: []string{}, Missing: []string{},
	}
	latest := map[string]*Signature{}
	at := map[string]time.Time{}
	for i, sg := range sigs {
		r := SignatureResult{Party: sg.Party, Decision: sg.Decision, KeyID: sg.KeyID, SignedAt: sg.SignedAt, Note: sg.Note}
		if i < len(sources) {
			r.Source = sources[i]
		}
		if err := sg.Check(st, lookup); err != nil {
			r.Problem = err.Error()
		} else {
			r.Valid = true
			t, _ := time.Parse(time.RFC3339, sg.SignedAt) // Check parsed it already
			if cur, ok := at[sg.Party]; !ok || t.After(cur) {
				latest[sg.Party], at[sg.Party] = sg, t
			}
		}
		rep.Signatures = append(rep.Signatures, r)
	}
	for _, party := range sortedKeys(latest) {
		if latest[party].Decision == DecisionAccept {
			rep.AcceptedBy = append(rep.AcceptedBy, party)
		} else {
			rep.DisputedBy = append(rep.DisputedBy, party)
		}
	}
	rep.OK = true
	for _, r := range rep.Signatures {
		if !r.Valid {
			rep.OK = false
		}
	}
	for _, party := range required {
		sg, ok := latest[party]
		switch {
		case !ok:
			rep.Missing = append(rep.Missing, party)
			rep.OK = false
		case sg.Decision != DecisionAccept:
			rep.OK = false
		}
	}
	return rep
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
