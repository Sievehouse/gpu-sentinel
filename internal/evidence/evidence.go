// Package evidence implements a tamper-evident, append-only evidence log.
//
// Every entry is one JSON line holding a canonicalised payload, the hash of
// the previous entry, and an Ed25519 signature over its own hash. Editing,
// deleting or reordering any entry breaks the chain; signing with an
// unexpected key fails verification. A Merkle root (RFC 6962 style) over the
// entry hashes lets a statement commit to the exact evidence it used.
//
// A log has a single writer. Run one log per agent and merge logs at read
// time; never let two processes append to the same file.
package evidence

import (
	"bufio"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/canonical"
)

const hashDomain = "capacity-ledger/evidence/v1\n"

// GenesisPrev is the prev value of the first entry in a log.
var GenesisPrev = strings.Repeat("0", 64)

// Entry is one line of an evidence log.
type Entry struct {
	Seq     int64           `json:"seq"`
	TS      string          `json:"ts"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
	Prev    string          `json:"prev"`
	KeyID   string          `json:"keyId"`
	Hash    string          `json:"hash"`
	Sig     string          `json:"sig"`
}

type hashedBody struct {
	Seq     int64           `json:"seq"`
	TS      string          `json:"ts"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
	Prev    string          `json:"prev"`
	KeyID   string          `json:"keyId"`
}

// ComputeHash returns the hex SHA-256 over the canonical entry body.
func (e Entry) ComputeHash() (string, error) {
	payload := e.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("null")
	}
	b, err := canonical.Marshal(hashedBody{e.Seq, e.TS, e.Kind, payload, e.Prev, e.KeyID})
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte(hashDomain))
	h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DecodePayload unmarshals the entry payload into v.
func (e Entry) DecodePayload(v any) error { return json.Unmarshal(e.Payload, v) }

// Writer appends signed entries to one log file.
type Writer struct {
	path      string
	priv      ed25519.PrivateKey
	keyID     string
	seq       int64
	prev      string
	now       func() time.Time
	announced bool
}

// OpenWriter prepares to append to path, which may not exist yet. It reads
// the last entry so that new entries continue the chain.
func OpenWriter(path string, priv ed25519.PrivateKey) (*Writer, error) {
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	w := &Writer{path: path, priv: priv, keyID: KeyID(pub), prev: GenesisPrev, now: time.Now}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := ReadEntries(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if n := len(entries); n > 0 {
		w.seq, w.prev = entries[n-1].Seq, entries[n-1].Hash
	}
	for _, e := range entries {
		if e.Kind == KindKey && e.KeyID == w.keyID {
			w.announced = true
		}
	}
	return w, nil
}

// KeyID returns the id of the signing key.
func (w *Writer) KeyID() string { return w.keyID }

// PublicKey returns the public half of the signing key.
func (w *Writer) PublicKey() ed25519.PublicKey { return w.priv.Public().(ed25519.PublicKey) }

// Announce appends a self-signed key announcement, unless this log already
// holds one for the writer's key. It reports whether an entry was written.
// Verifiers enrol the announced key in a keyring (capledger keyring import);
// an announcement on its own grants no trust.
func (w *Writer) Announce(node, role, software string) (bool, error) {
	if w.announced {
		return false, nil
	}
	pemText, err := PublicKeyPEM(w.PublicKey())
	if err != nil {
		return false, err
	}
	if _, err := w.Append(KindKey, KeyAnnouncement{KeyID: w.keyID, PublicKey: pemText, Node: node, Role: role, Software: software}); err != nil {
		return false, err
	}
	w.announced = true
	return true, nil
}

// NextSeq returns the sequence number the next entry will get.
func (w *Writer) NextSeq() int64 { return w.seq + 1 }

// SetClock replaces the clock used for entry timestamps (for tests).
func (w *Writer) SetClock(now func() time.Time) { w.now = now }

// Append canonicalises payload, signs the entry and appends it to the log.
func (w *Writer) Append(kind string, payload any) (Entry, error) {
	if strings.TrimSpace(kind) == "" {
		return Entry{}, errors.New("entry kind is required")
	}
	raw, err := canonical.Marshal(payload)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{
		Seq:     w.seq + 1,
		TS:      w.now().UTC().Format(time.RFC3339Nano),
		Kind:    kind,
		Payload: raw,
		Prev:    w.prev,
		KeyID:   w.keyID,
	}
	if e.Hash, err = e.ComputeHash(); err != nil {
		return Entry{}, err
	}
	digest, _ := hex.DecodeString(e.Hash)
	e.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(w.priv, digest))

	line, err := json.Marshal(e)
	if err != nil {
		return Entry{}, err
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return Entry{}, err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return Entry{}, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return Entry{}, err
	}
	if err := f.Close(); err != nil {
		return Entry{}, err
	}
	w.seq, w.prev = e.Seq, e.Hash
	return e, nil
}

// ReadEntries parses every line of a log without verifying it.
func ReadEntries(r io.Reader) ([]Entry, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var out []Entry
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(text), &e); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Report is the result of verifying a log.
type Report struct {
	Path       string   `json:"path,omitempty"`
	Entries    int      `json:"entries"`
	OK         bool     `json:"ok"`
	Problems   []string `json:"problems"`
	KeyIDs     []string `json:"keyIds"`
	Kinds      []string `json:"kinds"`
	LastHash   string   `json:"lastHash,omitempty"`
	MerkleRoot string   `json:"merkleRoot,omitempty"`
}

const maxProblems = 50

// Verify checks sequence numbers, the hash chain, every entry hash and every
// signature. keys maps key ids to trusted public keys.
func Verify(r io.Reader, keys map[string]ed25519.PublicKey) Report {
	return VerifyWith(r, keys, nil)
}

// EntryCheck is an extra policy check run on every correctly signed entry,
// for example a keyring's validity window. A non-nil error is reported as a
// problem for that entry.
type EntryCheck func(Entry) error

// VerifyWith is Verify plus an optional policy check per entry.
func VerifyWith(r io.Reader, keys map[string]ed25519.PublicKey, check EntryCheck) Report {
	rep := Report{Problems: []string{}, KeyIDs: []string{}, Kinds: []string{}}
	entries, err := ReadEntries(r)
	if err != nil {
		rep.Problems = append(rep.Problems, err.Error())
		return rep
	}
	problem := func(format string, a ...any) {
		if len(rep.Problems) < maxProblems {
			rep.Problems = append(rep.Problems, fmt.Sprintf(format, a...))
		}
	}
	prev := GenesisPrev
	expect := int64(1)
	keySeen, kindSeen := map[string]bool{}, map[string]bool{}
	leaves := make([][]byte, 0, len(entries))
	for _, e := range entries {
		if e.Seq != expect {
			problem("seq %d: expected seq %d (entries missing or reordered)", e.Seq, expect)
		}
		if e.Prev != prev {
			problem("seq %d: chain broken (prev does not match the previous entry's hash)", e.Seq)
		}
		if h, err := e.ComputeHash(); err != nil {
			problem("seq %d: cannot hash entry: %v", e.Seq, err)
		} else if h != e.Hash {
			problem("seq %d: content does not match its hash (entry was modified)", e.Seq)
		}
		digest, err := hex.DecodeString(e.Hash)
		if err != nil || len(digest) != sha256.Size {
			problem("seq %d: malformed hash", e.Seq)
		} else {
			leaves = append(leaves, digest)
			sig, err := base64.StdEncoding.DecodeString(e.Sig)
			pub, known := keys[e.KeyID]
			switch {
			case !known:
				problem("seq %d: signed by unknown key %s", e.Seq, e.KeyID)
			case err != nil || !ed25519.Verify(pub, digest, sig):
				problem("seq %d: invalid signature", e.Seq)
			case check != nil:
				if cerr := check(e); cerr != nil {
					problem("seq %d: %v", e.Seq, cerr)
				}
			}
		}
		if !keySeen[e.KeyID] {
			keySeen[e.KeyID] = true
			rep.KeyIDs = append(rep.KeyIDs, e.KeyID)
		}
		if !kindSeen[e.Kind] {
			kindSeen[e.Kind] = true
			rep.Kinds = append(rep.Kinds, e.Kind)
		}
		prev = e.Hash
		expect = e.Seq + 1
	}
	sort.Strings(rep.KeyIDs)
	sort.Strings(rep.Kinds)
	rep.Entries = len(entries)
	if len(entries) > 0 {
		rep.LastHash = entries[len(entries)-1].Hash
		rep.MerkleRoot = hex.EncodeToString(MerkleRoot(leaves))
	}
	rep.OK = len(rep.Problems) == 0
	return rep
}

// VerifyFile verifies the log at path.
func VerifyFile(path string, keys map[string]ed25519.PublicKey) Report {
	return VerifyFileWith(path, keys, nil)
}

// VerifyFileWith verifies the log at path with an extra policy check.
func VerifyFileWith(path string, keys map[string]ed25519.PublicKey, check EntryCheck) Report {
	f, err := os.Open(path)
	if err != nil {
		return Report{Path: path, Problems: []string{err.Error()}, KeyIDs: []string{}, Kinds: []string{}}
	}
	defer f.Close()
	rep := VerifyWith(f, keys, check)
	rep.Path = path
	return rep
}

// ReadFile parses every entry of the log at path without verifying it.
func ReadFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := ReadEntries(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return entries, nil
}
