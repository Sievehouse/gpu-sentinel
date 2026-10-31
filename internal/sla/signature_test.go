package sla

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"
)

type party struct {
	role string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newParty(t *testing.T, role string) party {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return party{role, pub, priv}
}

func lookupOf(parties ...party) SignerLookup {
	return func(id string, _ time.Time) (ed25519.PublicKey, string, error) {
		for _, p := range parties {
			if keyID(p.pub) == id {
				return p.pub, p.role, nil
			}
		}
		return nil, "", fmt.Errorf("key %s is not in the keyring", id)
	}
}

func exampleStatement(t *testing.T) *Statement {
	t.Helper()
	spec, evs := load(t, "../../examples/contracts/hgx-16-node.yaml", "../../examples/events/hgx-16-node-2026-10.jsonl")
	st, err := Compute(spec, evs, october)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCoSignedStatement(t *testing.T) {
	st := exampleStatement(t)
	prov, cust, fin := newParty(t, "provider"), newParty(t, "customer"), newParty(t, "financier")
	lookup := lookupOf(prov, cust, fin)
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	sp, err := Sign(st, "provider", DecisionAccept, "", prov.priv, at)
	if err != nil {
		t.Fatal(err)
	}
	required := []string{"provider", "customer"}
	rep := CheckCoSigned(st, []*Signature{sp}, nil, lookup, required)
	if rep.OK || len(rep.Missing) != 1 || rep.Missing[0] != "customer" {
		t.Fatalf("one signature must not be enough: %+v", rep)
	}
	sc, _ := Sign(st, "customer", DecisionAccept, "", cust.priv, at.Add(time.Hour))
	if rep := CheckCoSigned(st, []*Signature{sp, sc}, nil, lookup, required); !rep.OK || strings.Join(rep.AcceptedBy, ",") != "customer,provider" {
		t.Fatalf("co-signed: %+v", rep)
	}

	// The customer changes its mind: its latest signature counts.
	if _, err := Sign(st, "customer", DecisionDispute, "", cust.priv, at); err == nil {
		t.Fatal("a dispute without a note must be refused")
	}
	sd, _ := Sign(st, "customer", DecisionDispute, "gpu-009: 50 min seen only by our probe", cust.priv, at.Add(2*time.Hour))
	rep = CheckCoSigned(st, []*Signature{sp, sc, sd}, nil, lookup, required)
	if rep.OK || len(rep.DisputedBy) != 1 || rep.DisputedBy[0] != "customer" {
		t.Fatalf("dispute: %+v", rep)
	}

	// A key may sign only as its keyring role.
	forged, _ := Sign(st, "customer", DecisionAccept, "", fin.priv, at)
	if err := forged.Check(st, lookup); err == nil || !strings.Contains(err.Error(), `whose keyring role is "financier"`) {
		t.Fatalf("role not enforced: %v", err)
	}
	// Tampering breaks the signature.
	bad := *sp
	bad.Decision = DecisionDispute
	if err := bad.Check(st, lookup); err == nil || err.Error() != "invalid signature" {
		t.Fatalf("tampering not detected: %v", err)
	}
	// A signature belongs to one statement only.
	other := *st
	other.Credit.Percent = 0
	if err := other.Seal(); err != nil {
		t.Fatal(err)
	}
	if err := sp.Check(&other, lookup); err == nil || !strings.Contains(err.Error(), "signs statement") {
		t.Fatalf("statement binding: %v", err)
	}
	// Unknown keys fail.
	if err := sp.Check(st, lookupOf(cust)); err == nil {
		t.Fatal("unknown key accepted")
	}
	// Invalid signatures make the report fail even if every party accepted.
	if rep := CheckCoSigned(st, []*Signature{sp, sc, forged}, nil, lookup, required); rep.OK {
		t.Fatal("an invalid signature must fail the report")
	}
}

func TestSignRefusesUnsealedStatements(t *testing.T) {
	st := exampleStatement(t)
	st.Summary.AvailabilityPercent = "100.0000"
	p := newParty(t, "provider")
	if _, err := Sign(st, "provider", DecisionAccept, "", p.priv, time.Now()); err == nil {
		t.Fatal("signed a statement whose seal does not match")
	}
	if _, err := Sign(exampleStatement(t), "Provider!", DecisionAccept, "", p.priv, time.Now()); err == nil {
		t.Fatal("accepted a bad party name")
	}
}
