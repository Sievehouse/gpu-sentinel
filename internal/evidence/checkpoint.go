package evidence

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/canonical"
)

// Checkpoint document identifiers.
const (
	CheckpointAPIVersion = "capacity-ledger/v1alpha1"
	CheckpointKind       = "LogCheckpoint"
)

// checkpointDomain separates checkpoint signatures from every other signed
// object.
const checkpointDomain = "capacity-ledger/checkpoint/v1\n"

// LogHead is the state of one log at a moment: how many entries it held,
// the hash of the last one and the Merkle root over all of them.
type LogHead struct {
	Name       string `json:"name"`
	Entries    int64  `json:"entries"`
	LastHash   string `json:"lastHash"`
	MerkleRoot string `json:"merkleRoot"`
}

// Checkpoint is a signed set of log heads. Exchanging checkpoints (daily,
// say) between the parties, or having a neutral witness sign them, fixes
// every log's history up to that point: a log that is later rewritten no
// longer matches a checkpoint the other party already holds, even if the
// writer re-signs every entry.
type Checkpoint struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Logs       []LogHead `json:"logs"`
	CreatedAt  string    `json:"createdAt"`
	Note       string    `json:"note,omitempty"`
	KeyID      string    `json:"keyId"`
	Sig        string    `json:"sig"`
}

var logNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidLogName reports whether name can name a log in a checkpoint or on a
// ledger server: letters, digits, dot, underscore and hyphen, up to 128.
func ValidLogName(name string) bool { return logNameRe.MatchString(name) }

// LogName derives a log's name from its file path: the base name without a
// .jsonl extension.
func LogName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

// HeadOf computes the head of a log from its entries. It does not verify
// them; verify the log first.
func HeadOf(name string, entries []Entry) (LogHead, error) {
	h := LogHead{Name: name, Entries: int64(len(entries))}
	leaves, err := leavesOf(entries)
	if err != nil {
		return h, err
	}
	if len(entries) > 0 {
		h.LastHash = entries[len(entries)-1].Hash
	}
	h.MerkleRoot = hex.EncodeToString(MerkleRoot(leaves))
	return h, nil
}

func leavesOf(entries []Entry) ([][]byte, error) {
	leaves := make([][]byte, 0, len(entries))
	for _, e := range entries {
		d, err := hex.DecodeString(e.Hash)
		if err != nil || len(d) != sha256.Size {
			return nil, fmt.Errorf("seq %d: malformed hash", e.Seq)
		}
		leaves = append(leaves, d)
	}
	return leaves, nil
}

// CheckPrefix reports whether entries begin with exactly the history the
// head describes. A log that grew since the checkpoint passes; one whose
// first h.Entries entries changed, or that lost entries, does not.
func (h LogHead) CheckPrefix(entries []Entry) error {
	if int64(len(entries)) < h.Entries {
		return fmt.Errorf("log %s has %d entries but the checkpoint saw %d (entries were removed)", h.Name, len(entries), h.Entries)
	}
	prefix := entries[:h.Entries]
	got, err := HeadOf(h.Name, prefix)
	if err != nil {
		return err
	}
	if got.LastHash != h.LastHash || got.MerkleRoot != h.MerkleRoot {
		return fmt.Errorf("log %s: its first %d entries differ from the checkpoint (history was rewritten)", h.Name, h.Entries)
	}
	return nil
}

func (c *Checkpoint) digest() ([]byte, error) {
	cp := *c
	cp.Sig = ""
	b, err := canonical.Marshal(cp)
	if err != nil {
		return nil, err
	}
	sum := sha256.New()
	sum.Write([]byte(checkpointDomain))
	sum.Write(b)
	return sum.Sum(nil), nil
}

// NewCheckpoint signs the given heads at now. Heads are sorted by name; a
// name may appear once.
func NewCheckpoint(heads []LogHead, note string, priv ed25519.PrivateKey, now time.Time) (*Checkpoint, error) {
	if len(heads) == 0 {
		return nil, errors.New("a checkpoint needs at least one log")
	}
	hs := append([]LogHead(nil), heads...)
	sort.Slice(hs, func(i, j int) bool { return hs[i].Name < hs[j].Name })
	for i, h := range hs {
		if !ValidLogName(h.Name) {
			return nil, fmt.Errorf("log name %q: use letters, digits, '.', '_' and '-'", h.Name)
		}
		if i > 0 && hs[i-1].Name == h.Name {
			return nil, fmt.Errorf("log %q appears twice", h.Name)
		}
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	c := &Checkpoint{
		APIVersion: CheckpointAPIVersion, Kind: CheckpointKind, Logs: hs, Note: note,
		CreatedAt: now.UTC().Format(time.RFC3339), KeyID: KeyID(pub),
	}
	d, err := c.digest()
	if err != nil {
		return nil, err
	}
	c.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, d))
	return c, nil
}

// Verify checks the checkpoint's form and its signature with pub.
func (c *Checkpoint) Verify(pub ed25519.PublicKey) error {
	if c.APIVersion != CheckpointAPIVersion || c.Kind != CheckpointKind {
		return fmt.Errorf("not a %s %s document", CheckpointAPIVersion, CheckpointKind)
	}
	if KeyID(pub) != c.KeyID {
		return fmt.Errorf("checkpoint was signed by key %s, not %s", c.KeyID, KeyID(pub))
	}
	if _, err := time.Parse(time.RFC3339, c.CreatedAt); err != nil {
		return fmt.Errorf("createdAt: %w", err)
	}
	d, err := c.digest()
	if err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(c.Sig)
	if err != nil || !ed25519.Verify(pub, d, raw) {
		return errors.New("invalid checkpoint signature")
	}
	return nil
}

// Head returns the head for a log name, if the checkpoint covers it.
func (c *Checkpoint) Head(name string) (LogHead, bool) {
	for _, h := range c.Logs {
		if h.Name == name {
			return h, true
		}
	}
	return LogHead{}, false
}

// Time returns when the checkpoint was made.
func (c *Checkpoint) Time() time.Time {
	t, _ := time.Parse(time.RFC3339, c.CreatedAt)
	return t
}

// ReadCheckpoint loads a checkpoint file. Unknown fields are rejected.
func ReadCheckpoint(path string) (*Checkpoint, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := DecodeCheckpoint(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// DecodeCheckpoint parses a checkpoint document. Unknown fields are rejected.
func DecodeCheckpoint(b []byte) (*Checkpoint, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var c Checkpoint
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	return &c, nil
}
