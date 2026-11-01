package ledger

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/bundle"
	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
	"github.com/Sievehouse/gpu-sentinel/internal/sla"
)

// SnapshotType is the bundle type of a ledger snapshot.
const SnapshotType = "ledger-snapshot"

// snapshotPatterns are the files of a data directory a snapshot carries;
// temporary files and anything else are left out.
var snapshotPatterns = []string{
	"logs/*.jsonl", "logs/*.owner", "checkpoints/*.json", "statements/*.json", "signatures/*/*.json", "consents/*.json",
}

// Snapshot writes one contract's ledger data (logs, owners, checkpoints,
// statements, signatures and consents) to w as a signed evidence bundle, to
// carry a ledger across an air gap, back it up, or hand it to an auditor.
// Logs are cut at their last complete line, so a snapshot taken while the
// ledger runs is consistent per log.
func Snapshot(w io.Writer, dataDir, contract, note string, priv ed25519.PrivateKey, created time.Time) (*bundle.Manifest, error) {
	var inputs []bundle.Input
	for _, pat := range snapshotPatterns {
		matches, err := filepath.Glob(filepath.Join(dataDir, filepath.FromSlash(pat)))
		if err != nil {
			return nil, err
		}
		sort.Strings(matches)
		for _, m := range matches {
			rel, err := filepath.Rel(dataDir, m)
			if err != nil {
				return nil, err
			}
			name := filepath.ToSlash(rel)
			if strings.HasSuffix(name, ".jsonl") {
				b, err := os.ReadFile(m)
				if err != nil {
					return nil, err
				}
				if i := bytes.LastIndexByte(b, '\n'); i >= 0 {
					b = b[:i+1]
				} else {
					b = nil
				}
				inputs = append(inputs, bundle.BytesInput(name, b))
				continue
			}
			inputs = append(inputs, bundle.FileInput(name, m))
		}
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("%s holds no ledger data", dataDir)
	}
	if note == "" {
		note = "ledger snapshot"
	}
	return bundle.Create(w, bundle.Options{Type: SnapshotType, Subject: contract, Note: note, Created: created}, inputs, priv)
}

// RestoreReport says what a restored snapshot holds.
type RestoreReport struct {
	Contract    string   `json:"contract"`
	BundleID    string   `json:"bundleId"`
	Created     string   `json:"created"`
	Logs        int      `json:"logs"`
	Entries     int64    `json:"entries"`
	Checkpoints int      `json:"checkpoints"`
	Statements  int      `json:"statements"`
	Signatures  int      `json:"signatures"`
	Consents    int      `json:"consents"`
	Problems    []string `json:"problems"`
}

// Restore verifies a snapshot and writes it into dataDir, which must be new
// or empty. Nothing is written unless the bundle's signature, every log
// (chain, signatures, keyring rules), every checkpoint (signature, and that
// the logs extend it), every statement seal, every signature and every
// consent verify against kr. contract, if not empty, must match the
// snapshot's.
func Restore(bundlePath string, trust bundle.Trust, kr *keyring.Keyring, dataDir, contract string) (*RestoreReport, error) {
	if entries, err := os.ReadDir(dataDir); err == nil && len(entries) > 0 {
		return nil, fmt.Errorf("%s is not empty; restore into a new directory", dataDir)
	}
	parent := filepath.Dir(filepath.Clean(dataDir))
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(parent, ".capledger-restore-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	work := filepath.Join(tmp, "data")
	brep, err := bundle.ExtractFile(bundlePath, trust, work)
	if err != nil {
		if brep != nil && len(brep.Problems) > 0 {
			return nil, fmt.Errorf("%v: %s", err, strings.Join(brep.Problems, "; "))
		}
		return nil, err
	}
	rep := &RestoreReport{Contract: brep.Subject, BundleID: brep.BundleID, Created: brep.Created, Problems: []string{}}
	if brep.BundleType != SnapshotType {
		return nil, fmt.Errorf("%s is a %s bundle, not a %s", bundlePath, brep.BundleType, SnapshotType)
	}
	if contract != "" && brep.Subject != contract {
		return nil, fmt.Errorf("the snapshot is of contract %s, not %s", brep.Subject, contract)
	}
	for _, f := range brep.Files {
		if !snapshotFile(f.Path) {
			return nil, fmt.Errorf("unexpected file %s in a ledger snapshot", f.Path)
		}
	}
	problem := func(format string, a ...any) { rep.Problems = append(rep.Problems, fmt.Sprintf(format, a...)) }

	// Logs.
	logs := map[string][]evidence.Entry{}
	paths, _ := filepath.Glob(filepath.Join(work, "logs", "*.jsonl"))
	sort.Strings(paths)
	for _, p := range paths {
		name := evidence.LogName(p)
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		v := evidence.VerifyWith(bytes.NewReader(raw), kr.PublicKeys(), kr.EntryCheck())
		for _, pr := range v.Problems {
			problem("log %s: %s", name, pr)
		}
		entries, err := evidence.ReadEntries(bytes.NewReader(raw))
		if err != nil {
			problem("log %s: %v", name, err)
			continue
		}
		if _, err := os.Stat(strings.TrimSuffix(p, ".jsonl") + ".owner"); err != nil {
			problem("log %s has no owner file", name)
		}
		logs[name] = entries
		rep.Logs++
		rep.Entries += int64(len(entries))
	}
	// Checkpoints: signed by a trusted key, and every log still extends them.
	paths, _ = filepath.Glob(filepath.Join(work, "checkpoints", "*.json"))
	for _, p := range paths {
		cp, err := evidence.ReadCheckpoint(p)
		if err != nil {
			problem("checkpoint %s: %v", filepath.Base(p), err)
			continue
		}
		pub, _, err := kr.SignerAt(cp.KeyID, cp.Time())
		if err == nil {
			err = cp.Verify(pub)
		}
		if err != nil {
			problem("checkpoint %s: %v", filepath.Base(p), err)
			continue
		}
		for _, h := range cp.Logs {
			entries, ok := logs[h.Name]
			if !ok {
				problem("checkpoint %s covers log %s, which the snapshot lacks", filepath.Base(p), h.Name)
				continue
			}
			if err := h.CheckPrefix(entries); err != nil {
				problem("checkpoint %s: log %s: %v", filepath.Base(p), h.Name, err)
			}
		}
		rep.Checkpoints++
	}
	// Statements and their signatures.
	statements := map[string]*sla.Statement{}
	paths, _ = filepath.Glob(filepath.Join(work, "statements", "*.json"))
	for _, p := range paths {
		st, err := sla.ReadStatement(p)
		if err == nil {
			err = st.CheckSeal()
		}
		if err != nil {
			problem("statement %s: %v", filepath.Base(p), err)
			continue
		}
		if st.Contract.ID != brep.Subject {
			problem("statement %s is for contract %s", filepath.Base(p), st.Contract.ID)
		}
		statements[st.StatementSHA256] = st
		rep.Statements++
	}
	paths, _ = filepath.Glob(filepath.Join(work, "signatures", "*", "*.json"))
	for _, p := range paths {
		sha := filepath.Base(filepath.Dir(p))
		sg, err := sla.ReadSignature(p)
		if err != nil {
			problem("signature %s: %v", p[len(work)+1:], err)
			continue
		}
		st, ok := statements[sha]
		switch {
		case !ok:
			problem("signature %s is for a statement the snapshot lacks", filepath.ToSlash(p[len(work)+1:]))
		case sg.StatementSHA256 != sha:
			problem("signature %s is for statement %s", filepath.ToSlash(p[len(work)+1:]), sg.StatementSHA256)
		default:
			if err := sg.Check(st, kr.SignerAt); err != nil {
				problem("signature %s: %v", filepath.ToSlash(p[len(work)+1:]), err)
			}
		}
		rep.Signatures++
	}
	paths, _ = filepath.Glob(filepath.Join(work, "consents", "*.json"))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		c, err := DecodeConsent(b)
		if err == nil {
			err = c.Check(brep.Subject, kr.SignerAt)
		}
		if err != nil {
			problem("consent %s: %v", filepath.Base(p), err)
		}
		rep.Consents++
	}
	if len(rep.Problems) > 0 {
		return rep, errors.New("the snapshot did not verify; nothing was restored")
	}
	// The store must be able to open what it will serve.
	if _, err := OpenStore(work); err != nil {
		return rep, fmt.Errorf("the snapshot cannot be served: %w", err)
	}
	if err := os.Remove(dataDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return rep, err
	}
	if err := os.Rename(work, dataDir); err != nil {
		return rep, err
	}
	return rep, nil
}

func snapshotFile(p string) bool {
	for _, pat := range snapshotPatterns {
		if ok, _ := path.Match(pat, p); ok {
			return true
		}
	}
	return false
}
