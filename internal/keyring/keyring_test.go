package keyring

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
)

func newKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pemText, err := evidence.PublicKeyPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	return priv, pemText
}

func TestAddSaveLoadRoundTrip(t *testing.T) {
	_, pemA := newKey(t)
	_, pemB := newKey(t)
	k := New()
	if err := k.Add(Entry{PublicKey: pemA, Owner: "Example Neocloud", Role: RoleProviderAgent, Nodes: []string{"gpu-003"}}); err != nil {
		t.Fatal(err)
	}
	if err := k.Add(Entry{PublicKey: pemB, Owner: "Example Lab", Role: RoleTenantProbe, NotBefore: "2026-10-01T00:00:00Z", NotAfter: "2026-11-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keyring.json")
	if err := k.Save(path); err != nil {
		t.Fatal(err)
	}
	k2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(k2.Keys) != 2 || len(k2.PublicKeys()) != 2 {
		t.Fatalf("round trip lost keys: %+v", k2.Keys)
	}
	// Saved order: by owner, so the lab comes first.
	if k2.Keys[0].Owner != "Example Lab" {
		t.Fatalf("unexpected order %q", k2.Keys[0].Owner)
	}
	agent := k2.Keys[1]
	if !agent.AllowsNode("gpu-003") || agent.AllowsNode("gpu-004") {
		t.Fatal("node binding not enforced")
	}
	probe := k2.Keys[0]
	if !probe.AllowsNode("anything") {
		t.Fatal("unbound key should allow every node")
	}
	in := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	if probe.CheckTime(in) != nil || probe.CheckTime(in.AddDate(0, 1, 0)) == nil || probe.CheckTime(in.AddDate(0, -1, 0)) == nil {
		t.Fatal("validity window not enforced")
	}
}

func TestRejectsBadEntries(t *testing.T) {
	_, pemA := newKey(t)
	_, pemB := newKey(t)
	pubB, _ := evidence.ParsePublicKeyPEM(pemB)
	cases := map[string]Entry{
		"no owner":        {PublicKey: pemA, Role: "provider-agent"},
		"bad role":        {PublicKey: pemA, Owner: "x", Role: "Provider Agent"},
		"wrong key id":    {PublicKey: pemA, KeyID: evidence.KeyID(pubB), Owner: "x", Role: "harness"},
		"bad pem":         {PublicKey: "not a key", Owner: "x", Role: "harness"},
		"inverted window": {PublicKey: pemA, Owner: "x", Role: "harness", NotBefore: "2026-11-01T00:00:00Z", NotAfter: "2026-10-01T00:00:00Z"},
		"empty node":      {PublicKey: pemA, Owner: "x", Role: "harness", Nodes: []string{""}},
		"bad time":        {PublicKey: pemA, Owner: "x", Role: "harness", NotAfter: "soon"},
	}
	for name, e := range cases {
		if err := New().Add(e); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	k := New()
	if err := k.Add(Entry{PublicKey: pemA, Owner: "x", Role: "harness"}); err != nil {
		t.Fatal(err)
	}
	if err := k.Add(Entry{PublicKey: pemA, Owner: "y", Role: "auditor"}); err == nil || !strings.Contains(err.Error(), "already in the keyring") {
		t.Fatalf("duplicate key: %v", err)
	}
}

func TestAnnouncementEnrolmentAndPolicyChecks(t *testing.T) {
	dir := t.TempDir()
	priv, created, err := evidence.LoadOrCreateKey(filepath.Join(dir, "keys"))
	if err != nil || !created {
		t.Fatalf("key creation: %v created=%v", err, created)
	}
	again, created, err := evidence.LoadOrCreateKey(filepath.Join(dir, "keys"))
	if err != nil || created || !again.Equal(priv) {
		t.Fatal("second call must load the same key")
	}
	logPath := filepath.Join(dir, "evidence.jsonl")
	w, err := evidence.OpenWriter(logPath, priv)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 3, 4, 0, 0, 0, time.UTC)
	w.SetClock(func() time.Time { clock = clock.Add(time.Minute); return clock })
	if wrote, err := w.Announce("gpu-003", RoleProviderAgent, "test"); err != nil || !wrote {
		t.Fatalf("announce: %v %v", wrote, err)
	}
	if wrote, _ := w.Announce("gpu-003", RoleProviderAgent, "test"); wrote {
		t.Fatal("second announce must be a no-op")
	}
	if _, err := w.Append(evidence.KindEvent, map[string]string{"node": "gpu-003"}); err != nil {
		t.Fatal(err)
	}
	// Reopening the log remembers the announcement.
	w2, _ := evidence.OpenWriter(logPath, priv)
	if wrote, _ := w2.Announce("gpu-003", RoleProviderAgent, "test"); wrote {
		t.Fatal("reopened writer must see the earlier announcement")
	}

	entries, err := evidence.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	found, errs := FindAnnouncements(entries)
	if len(errs) != 0 || len(found) != 1 || found[0].Announcement.Node != "gpu-003" {
		t.Fatalf("announcements %+v errs %v", found, errs)
	}
	k := New()
	a := found[0].Announcement
	if err := k.Add(Entry{PublicKey: a.PublicKey, Owner: "Example Neocloud", Role: RoleProviderAgent, Nodes: []string{a.Node}, NotAfter: "2026-10-03T04:01:30Z"}); err != nil {
		t.Fatal(err)
	}
	// The announcement (04:01) is inside the window, the event (04:02) is not.
	rep := evidence.VerifyFileWith(logPath, k.PublicKeys(), k.EntryCheck())
	if rep.OK || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "after its notAfter") {
		t.Fatalf("expected one validity problem, got %+v", rep.Problems)
	}
	if err := k.CheckNode(entries[1], "gpu-004"); err == nil {
		t.Fatal("node binding not enforced for events")
	}

	// A forged announcement (payload swapped for another key) is rejected.
	_, otherPEM := newKey(t)
	forged := entries[0]
	forged.Payload = []byte(strings.Replace(string(forged.Payload), jsonEscape(a.PublicKey), jsonEscape(otherPEM), 1))
	if _, _, err := evidence.CheckAnnouncement(forged); err == nil {
		t.Fatal("forged announcement accepted")
	}
}

func jsonEscape(s string) string { return strings.ReplaceAll(s, "\n", `\n`) }

func TestFromPublicKeyFiles(t *testing.T) {
	dir := t.TempDir()
	_, pubPath, id, err := evidence.GenerateKeyPair(dir)
	if err != nil {
		t.Fatal(err)
	}
	k, err := FromPublicKeyFiles([]string{pubPath, pubPath}, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := k.Lookup(id); !ok || !e.AllowsNode("any") {
		t.Fatal("pub file key missing or bound")
	}
	if _, err := FromPublicKeyFiles([]string{filepath.Join(dir, "missing.pem")}, "cli"); err == nil {
		t.Fatal("missing file should fail")
	}
	_ = os.Remove(pubPath)
}

func TestEntryCheckEnforcesNodeBindings(t *testing.T) {
	dir := t.TempDir()
	priv, _, err := evidence.LoadOrCreateKey(filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "evidence.jsonl")
	w, _ := evidence.OpenWriter(logPath, priv)
	w.Announce("gpu-003", RoleProviderAgent, "test")
	w.Append(evidence.KindHeartbeat, map[string]any{"node": "gpu-003", "ts": "2026-10-03T04:00:00Z", "intervalSeconds": 60})
	w.Append(evidence.KindStatement, map[string]any{"node": "not-a-node-entry"})
	w.Append(evidence.KindEvent, map[string]any{"node": "gpu-004"})

	entries, _ := evidence.ReadFile(logPath)
	found, _ := FindAnnouncements(entries)
	k := New()
	if err := k.Add(Entry{PublicKey: found[0].Announcement.PublicKey, Owner: "Example Neocloud", Role: RoleProviderAgent, Nodes: []string{"gpu-003"}}); err != nil {
		t.Fatal(err)
	}
	rep := evidence.VerifyFileWith(logPath, k.PublicKeys(), k.EntryCheck())
	// Only the event for gpu-004 breaks the binding; statements are not
	// about a single node.
	if rep.OK || len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "seq 4") || !strings.Contains(rep.Problems[0], "for node gpu-004") {
		t.Fatalf("problems %v", rep.Problems)
	}
	if n, err := PayloadNode(entries[1]); err != nil || n != "gpu-003" {
		t.Fatalf("PayloadNode = %q, %v", n, err)
	}
	if e, err := k.KeyAt(entries[0].KeyID, time.Now()); err != nil || e.Public() == nil {
		t.Fatalf("KeyAt: %v", err)
	}
	if _, err := k.KeyAt("0000000000000000", time.Now()); err == nil {
		t.Fatal("unknown key accepted")
	}
}
