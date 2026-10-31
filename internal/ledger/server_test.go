package ledger

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/contract"
	"github.com/Sievehouse/gpu-sentinel/internal/events"
	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
	"github.com/Sievehouse/gpu-sentinel/internal/sla"
)

const contractID = "example-neocloud--example-lab--2026"

type world struct {
	t       *testing.T
	dir     string
	store   *Store
	server  *Server
	http    *httptest.Server
	kr      *keyring.Keyring
	witness ed25519.PublicKey
	keys    map[string]ed25519.PrivateKey // by keyring role
	clients map[string]*Client            // by party role
	logs    map[string]string             // log file by role
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, dir: t.TempDir(), kr: keyring.New(), keys: map[string]ed25519.PrivateKey{}, clients: map[string]*Client{}, logs: map[string]string{}}
	enrol := func(role, owner string, nodes ...string) {
		pub, priv := newKey(t)
		pemText, _ := evidence.PublicKeyPEM(pub)
		if err := w.kr.Add(keyring.Entry{PublicKey: pemText, Owner: owner, Role: role, Nodes: nodes}); err != nil {
			t.Fatal(err)
		}
		w.keys[role] = priv
	}
	enrol("provider-agent", "Example Neocloud", "gpu-003")
	enrol("tenant-probe", "Example Lab")
	enrol("provider", "Example Neocloud")
	enrol("customer", "Example Lab")
	enrol("financier", "Example Insurer")
	cfg := &Config{APIVersion: APIVersion, Kind: ConfigKind, Contract: contractID}
	for _, p := range []struct {
		name, role string
		write      []string
	}{
		{"example-neocloud", RoleProvider, []string{"provider-agent", "provider-integration"}},
		{"example-lab", RoleCustomer, []string{"tenant-probe"}},
		{"example-insurer", RoleFinancier, nil},
		{"example-auditor", RoleAuditor, nil},
	} {
		token := "token-of-" + p.name
		cfg.Parties = append(cfg.Parties, Party{Name: p.name, Role: p.role, TokenSHA256: TokenHash(token), WriteRoles: p.write})
		defer func(role, token string) {
			c, err := NewClient(w.http.URL, token)
			if err != nil {
				t.Fatal(err)
			}
			w.clients[role] = c
		}(p.role, token)
	}
	var err error
	if w.store, err = OpenStore(filepath.Join(w.dir, "data")); err != nil {
		t.Fatal(err)
	}
	var witness ed25519.PrivateKey
	w.witness, witness = newKey(t)
	if w.server, err = NewServer(w.store, cfg, w.kr, witness, nil); err != nil {
		t.Fatal(err)
	}
	w.http = httptest.NewServer(w.server.Handler())
	t.Cleanup(w.http.Close)
	return w
}

// write appends entries to a local log signed by the key of role.
func (w *world) write(role, file string, payloads ...map[string]any) []byte {
	w.t.Helper()
	path := filepath.Join(w.dir, file)
	lw, err := evidence.OpenWriter(path, w.keys[role])
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := lw.Announce("", role, "test"); err != nil {
		w.t.Fatal(err)
	}
	for _, p := range payloads {
		if _, err := lw.Append(evidence.KindEvent, p); err != nil {
			w.t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(path)
	return b
}

func lines(b []byte, from int) []byte {
	ls := strings.SplitAfter(strings.TrimRight(string(b), "\n"), "\n")
	return []byte(strings.Join(ls[from:], "") + "\n")
}

func status(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

func event(node, typ, ts string) map[string]any {
	return map[string]any{"id": node + typ + ts, "node": node, "type": typ, "ts": ts, "source": "capledger-agent"}
}

func TestAuthAndRoles(t *testing.T) {
	w := newWorld(t)
	resp, err := http.Get(w.http.URL + "/v1/logs")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	bad, _ := NewClient(w.http.URL, "guess")
	if _, err := bad.Info(); status(err) != http.StatusUnauthorized {
		t.Fatalf("bad token: %v", err)
	}
	in, err := w.clients[RoleCustomer].Info()
	if err != nil || in.Party.Name != "example-lab" || in.Contract != contractID || in.Witness.KeyID != evidence.KeyID(w.witness) {
		t.Fatalf("info %+v %v", in, err)
	}
	if _, err := w.clients[RoleFinancier].Logs(); status(err) != http.StatusForbidden {
		t.Fatalf("a financier must not read raw logs: %v", err)
	}
	if _, err := w.clients[RoleAuditor].Push("x", []byte("{}")); status(err) != http.StatusForbidden {
		t.Fatalf("an auditor must not push: %v", err)
	}
}

func TestPushPullAndOwnership(t *testing.T) {
	w := newWorld(t)
	prov, cust := w.clients[RoleProvider], w.clients[RoleCustomer]
	local := w.write("provider-agent", "gpu-003.jsonl", event("gpu-003", "down", "2026-10-03T04:12:00Z"), event("gpu-003", "up", "2026-10-03T06:40:00Z"))
	head, err := prov.Push("gpu-003", local)
	if err != nil {
		t.Fatal(err)
	}
	rep := evidence.VerifyFile(filepath.Join(w.dir, "gpu-003.jsonl"), w.kr.PublicKeys())
	if head.Entries != 3 || head.Owner != "example-neocloud" || head.MerkleRoot != rep.MerkleRoot || head.LastHash != rep.LastHash {
		t.Fatalf("head %+v, local %+v", head, rep)
	}
	// Pushing the same entries again does not continue the log.
	_, err = prov.Push("gpu-003", local)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusConflict || ae.Head == nil || ae.Head.Entries != 3 {
		t.Fatalf("replayed push: %v", err)
	}
	// The log grows; only the new entries are pushed.
	local = w.write("provider-agent", "gpu-003.jsonl", event("gpu-003", "down", "2026-10-09T10:00:00Z"))
	if head, err = prov.Push("gpu-003", lines(local, 3)); err != nil || head.Entries != 4 {
		t.Fatalf("continuation: %+v %v", head, err)
	}
	// Nobody else may write to the provider's log, ...
	probe := w.write("tenant-probe", "probe-gpu-003.jsonl", event("gpu-003", "down", "2026-10-03T04:13:00Z"))
	if _, err := cust.Push("gpu-003", probe); status(err) != http.StatusForbidden {
		t.Fatalf("customer wrote to the provider's log: %v", err)
	}
	// ... push logs signed by the other party's keys, ...
	if _, err := cust.Push("copy", local); status(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "may push only tenant-probe") {
		t.Fatalf("customer pushed provider entries: %v", err)
	}
	// ... or report for nodes the key is not bound to.
	stray := w.write("provider-agent", "stray.jsonl", event("gpu-004", "down", "2026-10-04T00:00:00Z"))
	if _, err := prov.Push("stray", stray); status(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "may speak only for gpu-003") {
		t.Fatalf("node binding: %v", err)
	}
	// Tampered entries and unknown keys are refused.
	tampered := bytes.Replace(probe, []byte("04:13:00"), []byte("05:13:00"), 1)
	if _, err := cust.Push("probe-gpu-003", tampered); status(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "does not match its hash") {
		t.Fatalf("tampering: %v", err)
	}
	_, strangerKey := newKey(t)
	w.keys["stranger"] = strangerKey
	if _, err := cust.Push("stranger", w.write("stranger", "stranger.jsonl")); status(err) != http.StatusBadRequest || !strings.Contains(err.Error(), "not in the ledger's keyring") {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := cust.Push("probe-gpu-003", probe); err != nil {
		t.Fatal(err)
	}
	if _, err := cust.Push("../escape", probe); err == nil {
		t.Fatal("a path-like log name was accepted")
	}

	// Either party reads every log, from any point.
	all, err := cust.Pull("gpu-003", 0)
	if err != nil || !bytes.Equal(all, local) {
		t.Fatalf("pull: %v\n%s", err, all)
	}
	if tail, _ := cust.Pull("gpu-003", 3); !bytes.Equal(tail, lines(local, 3)) {
		t.Fatalf("pull from 3:\n%s", tail)
	}
	if _, _, err := prov.Head("nope"); err != nil {
		t.Fatalf("missing head: %v", err)
	}
	// The store survives a restart.
	again, err := OpenStore(filepath.Join(w.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := again.Log("gpu-003"); got.LogHead != head.LogHead || got.Owner != "example-neocloud" {
		t.Fatalf("after restart: %+v", got)
	}
}

func TestWitnessCheckpoints(t *testing.T) {
	w := newWorld(t)
	prov := w.clients[RoleProvider]
	if _, err := prov.CreateCheckpoint(); status(err) != http.StatusConflict {
		t.Fatalf("checkpoint of nothing: %v", err)
	}
	local := w.write("provider-agent", "gpu-003.jsonl", event("gpu-003", "down", "2026-10-03T04:12:00Z"))
	if _, err := prov.Push("gpu-003", local); err != nil {
		t.Fatal(err)
	}
	res, err := w.clients[RoleCustomer].CreateCheckpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Checkpoint.Verify(w.witness); err != nil {
		t.Fatal(err)
	}
	entries, _ := evidence.ReadFile(filepath.Join(w.dir, "gpu-003.jsonl"))
	h, ok := res.Checkpoint.Head("gpu-003")
	if !ok || h.CheckPrefix(entries) != nil {
		t.Fatalf("checkpoint head %+v", h)
	}
	latest, err := w.clients[RoleAuditor].LatestCheckpoint()
	if err != nil || latest.ID != res.ID {
		t.Fatalf("latest %+v %v", latest, err)
	}
	// The periodic checkpoint skips unchanged logs.
	w.server.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	if res, err := w.server.Checkpoint(false); err != nil || res != nil {
		t.Fatalf("unchanged logs were checkpointed again: %+v %v", res, err)
	}
	local = w.write("provider-agent", "gpu-003.jsonl", event("gpu-003", "up", "2026-10-03T06:40:00Z"))
	if _, err := prov.Push("gpu-003", lines(local, 2)); err != nil {
		t.Fatal(err)
	}
	if res, err := w.server.Checkpoint(false); err != nil || res == nil {
		t.Fatalf("changed logs were not checkpointed: %v", err)
	}
}

func exampleStatement(t *testing.T) *sla.Statement {
	t.Helper()
	spec, err := contract.Load("../../examples/contracts/hgx-16-node.yaml")
	if err != nil {
		t.Fatal(err)
	}
	evs, err := events.ReadFile("../../examples/events/hgx-16-node-2026-10.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	st, err := sla.Compute(spec, evs, sla.Period{Start: 1790812800, End: 1793491200})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestStatementsSignaturesAndFeed(t *testing.T) {
	w := newWorld(t)
	prov, cust, fin := w.clients[RoleProvider], w.clients[RoleCustomer], w.clients[RoleFinancier]
	st := exampleStatement(t)
	if created, err := prov.PutStatement(st); err != nil || !created {
		t.Fatalf("put: %v", err)
	}
	if created, err := cust.PutStatement(st); err != nil || created {
		t.Fatalf("second put must be a no-op: %v", err)
	}
	tampered := *st
	tampered.Credit.Percent = 0
	if _, err := prov.PutStatement(&tampered); status(err) != http.StatusBadRequest {
		t.Fatalf("unsealed statement accepted: %v", err)
	}
	other := *st
	other.Contract.ID = "someone-else"
	other.Seal()
	if _, err := prov.PutStatement(&other); status(err) != http.StatusBadRequest {
		t.Fatalf("other contract accepted: %v", err)
	}

	now := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	sp, _ := sla.Sign(st, "provider", sla.DecisionAccept, "", w.keys["provider"], now)
	sc, _ := sla.Sign(st, "customer", sla.DecisionAccept, "", w.keys["customer"], now)
	if _, err := prov.PostSignature(sc); status(err) != http.StatusForbidden {
		t.Fatalf("provider uploaded the customer's signature: %v", err)
	}
	if rep, err := prov.PostSignature(sp); err != nil || rep.OK || rep.Missing[0] != "customer" {
		t.Fatalf("after provider: %+v %v", rep, err)
	}
	if rep, err := cust.PostSignature(sc); err != nil || !rep.OK {
		t.Fatalf("after customer: %+v %v", rep, err)
	}
	forged, _ := sla.Sign(st, "customer", sla.DecisionAccept, "", w.keys["provider"], now)
	if _, err := cust.PostSignature(forged); status(err) != http.StatusBadRequest {
		t.Fatalf("signature with the wrong role accepted: %v", err)
	}

	// The financier sees nothing until both parties consent.
	if _, err := fin.Feed(""); status(err) != http.StatusForbidden || !strings.Contains(err.Error(), "provider and customer") {
		t.Fatalf("closed feed: %v", err)
	}
	grant := func(c *Client, party, decision string, at time.Time) *ConsentResult {
		t.Helper()
		con, err := SignConsent(contractID, "example-insurer", party, decision, "", w.keys[party], at)
		if err != nil {
			t.Fatal(err)
		}
		res, err := c.PostConsent(con)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := grant(prov, "provider", Grant, now); res.FeedOpen || res.WaitingFor[0] != "customer" {
		t.Fatalf("after provider consent: %+v", res)
	}
	// The customer's key cannot consent on the provider's behalf.
	fake, _ := SignConsent(contractID, "example-insurer", "provider", Grant, "", w.keys["customer"], now)
	if _, err := prov.PostConsent(fake); status(err) != http.StatusBadRequest {
		t.Fatalf("consent with the wrong key: %v", err)
	}
	if _, err := cust.PostConsent(fake); status(err) != http.StatusForbidden {
		t.Fatalf("customer gave the provider's consent: %v", err)
	}
	if res := grant(cust, "customer", Grant, now); !res.FeedOpen {
		t.Fatalf("after both consents: %+v", res)
	}
	local := w.write("provider-agent", "gpu-003.jsonl", event("gpu-003", "down", "2026-10-03T04:12:00Z"))
	prov.Push("gpu-003", local)
	prov.CreateCheckpoint()
	feed, err := fin.Feed("")
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Statements) != 1 || !feed.Statements[0].CoSigned || len(feed.Statements[0].Signatures) != 2 || len(feed.Checkpoints) != 1 {
		t.Fatalf("feed %+v", feed)
	}
	if err := feed.Statements[0].Statement.CheckSeal(); err != nil {
		t.Fatal(err)
	}
	if feed.Witness.KeyID != evidence.KeyID(w.witness) || feed.Checkpoints[0].Checkpoint.Verify(w.witness) != nil {
		t.Fatal("feed checkpoints must verify with the witness key")
	}
	// A financier may countersign while its feed is open.
	sf, _ := sla.Sign(st, "financier", sla.DecisionAccept, "", w.keys["financier"], now)
	if _, err := fin.PostSignature(sf); err != nil {
		t.Fatal(err)
	}
	// Parties can preview a financier's feed.
	if _, err := cust.Feed("example-insurer"); err != nil {
		t.Fatal(err)
	}
	if _, err := cust.Feed(""); status(err) != http.StatusBadRequest {
		t.Fatalf("preview without a financier: %v", err)
	}
	// Either party can close the feed again.
	grant(cust, "customer", Revoke, now.Add(time.Hour))
	if _, err := fin.Feed(""); status(err) != http.StatusForbidden {
		t.Fatalf("revoked feed still open: %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	ok := func() Config {
		return Config{APIVersion: APIVersion, Kind: ConfigKind, Contract: "c", Parties: []Party{
			{Name: "p", Role: RoleProvider, TokenSHA256: TokenHash("a"), WriteRoles: []string{"provider-agent"}},
			{Name: "c", Role: RoleCustomer, TokenSHA256: TokenHash("b")},
		}}
	}
	base := ok()
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		edit func(*Config)
		want string
	}{
		{func(c *Config) { c.Parties[1].TokenSHA256 = c.Parties[0].TokenSHA256 }, "shares a token"},
		{func(c *Config) { c.Parties[1].Role = RoleProvider }, "exactly one provider and one customer"},
		{func(c *Config) { c.Parties[1].Name = "p" }, "listed twice"},
		{func(c *Config) { c.Parties[0].TokenSHA256 = "secret" }, "64 lowercase hex"},
		{func(c *Config) {
			c.Parties = append(c.Parties, Party{Name: "f", Role: RoleFinancier, TokenSHA256: TokenHash("f"), WriteRoles: []string{"x"}})
		}, "pushes no logs"},
		{func(c *Config) { c.Contract = "" }, "contract"},
	}
	for _, tc := range cases {
		c := ok()
		tc.edit(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("want %q, got %v", tc.want, err)
		}
	}
	if p, ok := base.ByToken("b"); !ok || p.Name != "c" {
		t.Fatal("token lookup")
	}
	if _, ok := base.ByToken(""); ok {
		t.Fatal("empty token matched")
	}
}

func TestExampleConfigLoads(t *testing.T) {
	c, err := LoadConfig("../../deploy/ledger/ledger.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := c.ByRole(RoleCustomer); !ok || p.Name != "example-lab" || !p.MayWrite("tenant-probe") {
		t.Fatalf("customer %+v", p)
	}
}

func TestReload(t *testing.T) {
	w := newWorld(t)
	cfg := *w.server.state.Load().cfg
	cfg.Contract = "another"
	if err := w.server.Reload(&cfg, w.kr); err == nil {
		t.Fatal("contract switch accepted")
	}
	// After a reload without the agent's key, its entries are refused.
	cfg.Contract = contractID
	if err := w.server.Reload(&cfg, keyring.New()); err != nil {
		t.Fatal(err)
	}
	local := w.write("provider-agent", "gpu-003.jsonl")
	if _, err := w.clients[RoleProvider].Push("gpu-003", local); status(err) != http.StatusBadRequest {
		t.Fatalf("key removed from the keyring still accepted: %v", err)
	}
}
