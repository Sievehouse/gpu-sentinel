package ledger

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
)

// hubContract builds a contract whose provider agent key is enrolled in the
// contract's own keyring, and returns the key and the provider's token.
func hubContract(t *testing.T, id string) (HubContract, ed25519.PrivateKey, string) {
	t.Helper()
	kr := keyring.New()
	pub, priv := newKey(t)
	pemText, _ := evidence.PublicKeyPEM(pub)
	if err := kr.Add(keyring.Entry{PublicKey: pemText, Owner: "provider of " + id, Role: "provider-agent"}); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{APIVersion: APIVersion, Kind: ConfigKind, Contract: id, Parties: []Party{
		{Name: "provider", Role: RoleProvider, TokenSHA256: TokenHash("p-" + id), WriteRoles: []string{"provider-agent"}},
		{Name: "customer", Role: RoleCustomer, TokenSHA256: TokenHash("c-" + id), WriteRoles: []string{"tenant-probe"}},
	}}
	return HubContract{Config: cfg, Keyring: kr}, priv, "p-" + id
}

func signedLog(t *testing.T, dir string, priv ed25519.PrivateKey) []byte {
	t.Helper()
	path := filepath.Join(dir, evidence.KeyID(priv.Public().(ed25519.PublicKey))+".jsonl")
	lw, err := evidence.OpenWriter(path, priv)
	if err != nil {
		t.Fatal(err)
	}
	lw.Announce("", "provider-agent", "test")
	lw.Append(evidence.KindEvent, event("gpu-003", "down", "2026-10-03T04:12:00Z"))
	b, _ := os.ReadFile(path)
	return b
}

func TestHub(t *testing.T) {
	dir := t.TempDir()
	_, witness := newKey(t)
	hub := NewHub(filepath.Join(dir, "data"), witness, nil)
	a, keyA, tokA := hubContract(t, "neocloud--lab-a--2026")
	b, keyB, tokB := hubContract(t, "neocloud--lab-b--2026")
	added, removed, err := hub.Apply([]HubContract{a, b})
	if err != nil || len(added) != 2 || len(removed) != 0 {
		t.Fatalf("apply: %v %v %v", added, removed, err)
	}
	srv := httptest.NewServer(hub.Handler())
	defer srv.Close()
	client := func(id, token string) *Client {
		c, err := NewClient(srv.URL+"/c/"+id, token)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ca, cb := client(a.Config.Contract, tokA), client(b.Config.Contract, tokB)

	info, err := ca.Info()
	if err != nil || info.Contract != a.Config.Contract {
		t.Fatalf("info: %+v %v", info, err)
	}
	logA := signedLog(t, dir, keyA)
	if _, err := ca.Push("agent-a", logA); err != nil {
		t.Fatalf("push to own contract: %v", err)
	}
	// Contracts are separate: B's provider cannot read A, A's token does
	// not open B, and a key enrolled only for A cannot write to B.
	if _, err := client(a.Config.Contract, tokB).Logs(); status(err) != http.StatusUnauthorized {
		t.Fatalf("cross-contract token: %v", err)
	}
	if logs, err := cb.Logs(); err != nil || len(logs) != 0 {
		t.Fatalf("B sees A's logs: %v %v", logs, err)
	}
	if _, err := cb.Push("agent-a", logA); err == nil || !strings.Contains(err.Error(), "not in the ledger's keyring") {
		t.Fatalf("A's key accepted by B: %v", err)
	}
	if _, err := cb.Push("agent-b", signedLog(t, dir, keyB)); err != nil {
		t.Fatal(err)
	}
	// Unknown contracts look like unknown tokens.
	for _, path := range []string{"/c/nope/v1/info", "/v1/info", "/c/"} {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tokA)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: HTTP %d", path, resp.StatusCode)
		}
	}
	if resp, err := http.Get(srv.URL + "/healthz"); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz: %v", err)
	}
	// Each contract's checkpoint names its contract.
	hub.SetClock(func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) })
	res, err := hub.Checkpoint(false)
	if err != nil || len(res) != 2 {
		t.Fatalf("checkpoint: %v %v", res, err)
	}
	if got := res[a.Config.Contract].Checkpoint.Note; !strings.Contains(got, a.Config.Contract) {
		t.Fatalf("checkpoint note %q does not name the contract", got)
	}
	if res, _ := hub.Checkpoint(false); len(res) != 0 {
		t.Fatalf("unchanged logs checkpointed again: %v", res)
	}
	// Data lives per contract.
	if _, err := os.Stat(filepath.Join(dir, "data", "contracts", a.Config.Contract, "logs", "agent-a.jsonl")); err != nil {
		t.Fatal(err)
	}

	// Removing B stops serving it; adding it back serves its data again.
	if _, removed, err := hub.Apply([]HubContract{a}); err != nil || len(removed) != 1 {
		t.Fatalf("remove: %v %v", removed, err)
	}
	if _, err := cb.Logs(); status(err) != http.StatusUnauthorized {
		t.Fatalf("removed contract still served: %v", err)
	}
	if _, _, err := hub.Apply([]HubContract{a, b}); err != nil {
		t.Fatal(err)
	}
	if logs, err := cb.Logs(); err != nil || len(logs) != 1 {
		t.Fatalf("re-added contract: %v %v", logs, err)
	}

	// Invalid sets change nothing.
	bad, _, _ := hubContract(t, "../escape")
	if _, _, err := hub.Apply([]HubContract{a, bad}); err == nil {
		t.Fatal("path-like contract id accepted")
	}
	if _, _, err := hub.Apply([]HubContract{a, a}); err == nil {
		t.Fatal("duplicate contract accepted")
	}
	if got := hub.Contracts(); len(got) != 2 {
		t.Fatalf("a failed apply changed the hub: %v", got)
	}
}

func TestConfigKeyringPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.json")
	os.WriteFile(path, []byte(`{"apiVersion":"capacity-ledger/v1alpha1","kind":"LedgerConfig","contract":"c","keyring":"keys/a.json","parties":[
 {"name":"p","role":"provider","tokenSha256":"`+TokenHash("p")+`"},
 {"name":"c","role":"customer","tokenSha256":"`+TokenHash("c")+`"}]}`), 0o644)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.KeyringPath(); got != filepath.Join(dir, "keys", "a.json") {
		t.Fatalf("keyring path %q", got)
	}
}
