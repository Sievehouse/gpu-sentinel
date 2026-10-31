package evidence

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCheckpointFixesHistory(t *testing.T) {
	w, path, _ := newWriter(t)
	appendSome(t, w)
	entries, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head, err := HeadOf(LogName(path), entries)
	if err != nil {
		t.Fatal(err)
	}
	if head.Name != "evidence" || head.Entries != 3 || head.LastHash != entries[2].Hash {
		t.Fatalf("head %+v", head)
	}
	rep := VerifyFile(path, map[string]ed25519.PublicKey{w.KeyID(): w.PublicKey()})
	if head.MerkleRoot != rep.MerkleRoot {
		t.Fatalf("checkpoint root %s differs from the verifier's %s", head.MerkleRoot, rep.MerkleRoot)
	}

	wpub, wpriv, _ := ed25519.GenerateKey(rand.Reader)
	cp, err := NewCheckpoint([]LogHead{head}, "daily exchange", wpriv, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Verify(wpub); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(t.TempDir(), "cp.json")
	b, _ := json.MarshalIndent(cp, "", "  ")
	os.WriteFile(saved, b, 0o644)
	cp, err = ReadCheckpoint(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := cp.Verify(wpub); err != nil {
		t.Fatalf("after a round trip: %v", err)
	}
	h, ok := cp.Head("evidence")
	if !ok {
		t.Fatal("head missing")
	}

	// The log keeps growing: still consistent with the checkpoint.
	if _, err := w.Append("event", map[string]any{"node": "gpu-003", "i": 4}); err != nil {
		t.Fatal(err)
	}
	grown, _ := ReadFile(path)
	if err := h.CheckPrefix(grown); err != nil {
		t.Fatalf("a grown log must pass: %v", err)
	}

	// The writer rewrites its history with the same key, re-signing every
	// entry: the signatures verify, but the checkpoint no longer matches.
	priv := w.priv
	os.Remove(path)
	w2, err := OpenWriter(path, priv)
	if err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{"observation", "event", "statement"} {
		w2.Append(kind, map[string]any{"i": i, "node": "gpu-004"})
	}
	rewritten, _ := ReadFile(path)
	if rep := VerifyFile(path, map[string]ed25519.PublicKey{w.KeyID(): w.PublicKey()}); !rep.OK {
		t.Fatalf("the rewritten log is validly signed: %v", rep.Problems)
	}
	if err := h.CheckPrefix(rewritten); err == nil || !strings.Contains(err.Error(), "history was rewritten") {
		t.Fatalf("rewrite not caught: %v", err)
	}
	if err := h.CheckPrefix(rewritten[:2]); err == nil || !strings.Contains(err.Error(), "entries were removed") {
		t.Fatalf("truncation not caught: %v", err)
	}

	// Tampering with the checkpoint breaks its signature.
	cp.Logs[0].Entries = 2
	if err := cp.Verify(wpub); err == nil {
		t.Fatal("tampered checkpoint verified")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := cp.Verify(other); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestCheckpointRejectsBadHeads(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	if _, err := NewCheckpoint(nil, "", priv, now); err == nil {
		t.Fatal("empty checkpoint accepted")
	}
	if _, err := NewCheckpoint([]LogHead{{Name: "a"}, {Name: "a"}}, "", priv, now); err == nil {
		t.Fatal("duplicate log accepted")
	}
	if _, err := NewCheckpoint([]LogHead{{Name: "../etc"}}, "", priv, now); err == nil {
		t.Fatal("bad name accepted")
	}
	if LogName("/var/lib/x/gpu-003.jsonl") != "gpu-003" || !ValidLogName("probe-gpu-003") || ValidLogName("a/b") {
		t.Fatal("log names")
	}
}
