package ledger

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/sla"
)

// Store keeps a ledger's data as plain files under one directory, so that
// it can be backed up, inspected and verified with the ordinary capledger
// commands:
//
//	logs/NAME.jsonl            evidence logs, append-only
//	logs/NAME.owner            the party that pushed the log first
//	checkpoints/ID.json        signed checkpoints, ID = time and key id
//	statements/SHA.json        sealed statements
//	signatures/SHA/N.json      statement signatures, in arrival order
//	consents/N.json            feed consents, in arrival order
//
// One process owns a data directory. The store is safe for concurrent use
// within that process.
type Store struct {
	dir  string
	mu   sync.Mutex
	logs map[string]*logState
}

type logState struct {
	owner  string
	head   evidence.LogHead
	leaves [][]byte
}

// LogInfo describes one stored log.
type LogInfo struct {
	evidence.LogHead
	Owner string `json:"owner"`
}

// ErrConflict means a push does not continue the stored log.
var ErrConflict = errors.New("conflict")

// ErrNotFound means the requested object does not exist.
var ErrNotFound = errors.New("not found")

var subdirs = []string{"logs", "checkpoints", "statements", "signatures", "consents"}

// OpenStore opens (creating if needed) the data directory and loads every
// log's head.
func OpenStore(dir string) (*Store, error) {
	for _, d := range subdirs {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o750); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir, logs: map[string]*logState{}}
	paths, err := filepath.Glob(filepath.Join(dir, "logs", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	for _, p := range paths {
		name := evidence.LogName(p)
		entries, err := evidence.ReadFile(p)
		if err != nil {
			return nil, err
		}
		head, err := evidence.HeadOf(name, entries)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		leaves := make([][]byte, 0, len(entries))
		for _, e := range entries {
			d, _ := hex.DecodeString(e.Hash) // HeadOf checked every hash
			leaves = append(leaves, d)
		}
		owner, err := os.ReadFile(filepath.Join(dir, "logs", name+".owner"))
		if err != nil {
			return nil, fmt.Errorf("log %s has no owner file: %w", name, err)
		}
		s.logs[name] = &logState{owner: strings.TrimSpace(string(owner)), head: head, leaves: leaves}
	}
	return s, nil
}

// Logs returns every log, sorted by name.
func (s *Store) Logs() []LogInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LogInfo, 0, len(s.logs))
	for _, l := range s.logs {
		out = append(out, LogInfo{LogHead: l.head, Owner: l.owner})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Log returns one log's head and owner.
func (s *Store) Log(name string) (LogInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.logs[name]
	if !ok {
		return LogInfo{LogHead: evidence.LogHead{Name: name}}, false
	}
	return LogInfo{LogHead: l.head, Owner: l.owner}, true
}

// Append adds entries to a log after check approves them against the
// current head, under the store lock, so two pushes to the same log cannot
// interleave. A new log is created with owner as its owner.
func (s *Store) Append(name, owner string, entries []evidence.Entry, check func(cur LogInfo, exists bool) error) (LogInfo, error) {
	if !evidence.ValidLogName(name) {
		return LogInfo{}, fmt.Errorf("log name %q: use letters, digits, '.', '_' and '-'", name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	l, exists := s.logs[name]
	cur := LogInfo{LogHead: evidence.LogHead{Name: name}}
	if exists {
		cur = LogInfo{LogHead: l.head, Owner: l.owner}
	}
	if err := check(cur, exists); err != nil {
		return cur, err
	}
	var buf bytes.Buffer
	leaves := make([][]byte, 0, len(entries))
	for _, e := range entries {
		line, err := json.Marshal(e)
		if err != nil {
			return cur, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
		d, err := hex.DecodeString(e.Hash)
		if err != nil {
			return cur, err
		}
		leaves = append(leaves, d)
	}
	if !exists {
		if err := writeFileAtomic(filepath.Join(s.dir, "logs", name+".owner"), []byte(owner+"\n")); err != nil {
			return cur, err
		}
		l = &logState{owner: owner, head: evidence.LogHead{Name: name}}
	}
	f, err := os.OpenFile(filepath.Join(s.dir, "logs", name+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return cur, err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return cur, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return cur, err
	}
	if err := f.Close(); err != nil {
		return cur, err
	}
	l.leaves = append(l.leaves, leaves...)
	l.head = evidence.LogHead{
		Name: name, Entries: int64(len(l.leaves)), LastHash: entries[len(entries)-1].Hash,
		MerkleRoot: hex.EncodeToString(evidence.MerkleRoot(l.leaves)),
	}
	s.logs[name] = l
	return LogInfo{LogHead: l.head, Owner: l.owner}, nil
}

// CopyLog writes a log's entries from index from (0-based) onwards to w.
func (s *Store) CopyLog(name string, from int64, w io.Writer) error {
	s.mu.Lock()
	l, ok := s.logs[name]
	var n int64
	if ok {
		n = l.head.Entries
	}
	s.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	f, err := os.Open(filepath.Join(s.dir, "logs", name+".jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var i int64
	for sc.Scan() && i < n { // never send a line written after the head was read
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		if i >= from {
			if _, err := w.Write(append(sc.Bytes(), '\n')); err != nil {
				return err
			}
		}
		i++
	}
	return sc.Err()
}

// Heads returns every log's head, for a checkpoint.
func (s *Store) Heads() []evidence.LogHead {
	logs := s.Logs()
	out := make([]evidence.LogHead, 0, len(logs))
	for _, l := range logs {
		out = append(out, l.LogHead)
	}
	return out
}

// CheckpointID names a checkpoint file: its time and its signer.
func CheckpointID(cp *evidence.Checkpoint) string {
	return cp.Time().UTC().Format("20060102T150405Z") + "-" + cp.KeyID
}

// SaveCheckpoint stores a checkpoint and returns its id.
func (s *Store) SaveCheckpoint(cp *evidence.Checkpoint) (string, error) {
	id := CheckpointID(cp)
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return id, writeFileAtomic(filepath.Join(s.dir, "checkpoints", id+".json"), append(b, '\n'))
}

// Checkpoints returns the ids of all checkpoints, oldest first.
func (s *Store) Checkpoints() ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(s.dir, "checkpoints", "*.json"))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(paths))
	for _, p := range paths {
		ids = append(ids, strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	sort.Strings(ids)
	return ids, nil
}

// Checkpoint loads one checkpoint.
func (s *Store) Checkpoint(id string) (*evidence.Checkpoint, error) {
	if !evidence.ValidLogName(id) {
		return nil, ErrNotFound
	}
	cp, err := evidence.ReadCheckpoint(filepath.Join(s.dir, "checkpoints", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return cp, err
}

var shaRe = hexRe

// PutStatement stores a sealed statement. It reports whether it was new.
func (s *Store) PutStatement(st *sla.Statement) (bool, error) {
	p := filepath.Join(s.dir, "statements", st.StatementSHA256+".json")
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(p); err == nil {
		return false, nil
	}
	return true, writeFileAtomic(p, append(b, '\n'))
}

// Statement loads one statement by its hash.
func (s *Store) Statement(sha string) (*sla.Statement, error) {
	if !shaRe.MatchString(sha) {
		return nil, ErrNotFound
	}
	st, err := sla.ReadStatement(filepath.Join(s.dir, "statements", sha+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return st, err
}

// Statements returns every stored statement, ordered by period then hash.
func (s *Store) Statements() ([]*sla.Statement, error) {
	paths, err := filepath.Glob(filepath.Join(s.dir, "statements", "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]*sla.Statement, 0, len(paths))
	for _, p := range paths {
		st, err := sla.ReadStatement(p)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Period.Start != out[j].Period.Start {
			return out[i].Period.Start < out[j].Period.Start
		}
		return out[i].StatementSHA256 < out[j].StatementSHA256
	})
	return out, nil
}

// AddSignature stores a signature for a stored statement.
func (s *Store) AddSignature(sg *sla.Signature) error {
	dir := filepath.Join(s.dir, "signatures", sg.StatementSHA256)
	b, err := json.MarshalIndent(sg, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	return appendNumbered(dir, append(b, '\n'))
}

// Signatures returns a statement's signatures in arrival order.
func (s *Store) Signatures(sha string) ([]*sla.Signature, error) {
	if !shaRe.MatchString(sha) {
		return nil, ErrNotFound
	}
	var out []*sla.Signature
	err := readNumbered(filepath.Join(s.dir, "signatures", sha), func(b []byte) error {
		sg, err := sla.DecodeSignature(b)
		if err == nil {
			out = append(out, sg)
		}
		return err
	})
	return out, err
}

// AddConsent stores a consent.
func (s *Store) AddConsent(c *Consent) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return appendNumbered(filepath.Join(s.dir, "consents"), append(b, '\n'))
}

// Consents returns every consent in arrival order.
func (s *Store) Consents() ([]*Consent, error) {
	var out []*Consent
	err := readNumbered(filepath.Join(s.dir, "consents"), func(b []byte) error {
		c, err := DecodeConsent(b)
		if err == nil {
			out = append(out, c)
		}
		return err
	})
	return out, err
}

// appendNumbered writes b as the next file 000001.json, 000002.json, ...
// The caller holds the store lock.
func appendNumbered(dir string, b []byte) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, fmt.Sprintf("%06d.json", len(paths)+1)), b)
}

func readNumbered(dir string, fn func([]byte) error) error {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// writeFileAtomic writes via a temporary file and a rename, so readers
// never see a partial file.
func writeFileAtomic(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o640); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
