package main

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/bundle"
	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
)

func cmdBundleCreate(args []string, s streams) error {
	fs := newFlags("bundle create", s)
	out := fs.String("out", "", "bundle file to write, e.g. acceptance-2026-10-07.tar.gz")
	typ := fs.String("type", bundle.TypeAcceptance, "bundle type: acceptance, statement, incident or another lowercase slug")
	subject := fs.String("subject", "", "what the evidence is about, e.g. a cluster or contract id")
	note := fs.String("note", "", "free-text note")
	created := fs.String("created", "", "creation time (RFC 3339); default now")
	var files, dirs stringList
	fs.Var(&files, "file", "file to include under its base name, or NAME=PATH to choose its path in the bundle; repeat")
	fs.Var(&dirs, "dir", "directory to include, recursively, under its base name; repeat")
	keyPath := fs.String("key", "", "private signing key (PEM)")
	keyDir := fs.String("key-dir", "", "directory holding the signing key; created on first use (instead of --key)")
	logPath := fs.String("log", "", "also record the bundle id in this evidence log")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("out", *out, "subject", *subject); err != nil {
		return err
	}
	if len(files) == 0 && len(dirs) == 0 {
		return usagef("give at least one --file or --dir")
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite a bundle", *out)
	}
	when := time.Now()
	if *created != "" {
		t, err := time.Parse(time.RFC3339, *created)
		if err != nil {
			return usagef("--created: %v", err)
		}
		when = t
	}
	var inputs []bundle.Input
	for _, f := range files {
		name, path, explicit := strings.Cut(f, "=")
		if !explicit {
			name, path = filepath.Base(f), f
		}
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file (use --dir for directories)", path)
		}
		if err := bundle.CheckPath(name); err != nil {
			if !explicit {
				return fmt.Errorf("%w; choose a name with --file NAME=%s", err, path)
			}
			return err
		}
		inputs = append(inputs, bundle.FileInput(name, path))
	}
	for _, d := range dirs {
		ins, err := bundle.DirInputs(filepath.Base(filepath.Clean(d)), d)
		if err != nil {
			return err
		}
		inputs = append(inputs, ins...)
	}
	priv, err := signingKey(*keyPath, *keyDir, s)
	if err != nil {
		return err
	}

	// Write to a temporary file and rename, so a failed run never leaves a
	// partial bundle behind.
	tmp, err := os.CreateTemp(filepath.Dir(*out), ".capledger-bundle-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	m, err := bundle.Create(tmp, bundle.Options{Type: *typ, Subject: *subject, Note: *note, Created: when}, inputs, priv)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), *out); err != nil {
		return err
	}
	id, err := m.ID()
	if err != nil {
		return err
	}
	fmt.Fprintf(s.out, "wrote %s: %d files, %d bytes, signed by key %s\nbundle id %s\n", *out, len(m.Files), m.TotalBytes(), m.Signer.KeyID, id)
	if *logPath != "" {
		w, err := openLog(*logPath, priv, "", "")
		if err != nil {
			return err
		}
		rec, err := m.LogRecord()
		if err != nil {
			return err
		}
		entry, err := w.Append(evidence.KindBundle, rec)
		if err != nil {
			return err
		}
		fmt.Fprintf(s.out, "recorded in %s (seq %d, hash %s)\n", *logPath, entry.Seq, entry.Hash)
	}
	return nil
}

func cmdBundleVerify(args []string, s streams) error {
	fs := newFlags("bundle verify", s)
	path := fs.String("bundle", "", "bundle file (.tar.gz)")
	var pubs stringList
	fs.Var(&pubs, "pub", "trusted public key (PEM); repeat as needed")
	keyringPath := fs.String("keyring", "", "keyring of trusted keys (validity windows apply at the bundle's creation time)")
	extract := fs.String("extract", "", "after verifying, write the files into this new or empty directory")
	format := fs.String("format", "text", "text or json")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("bundle", *path); err != nil {
		return err
	}
	if err := checkFormat(*format); err != nil {
		return err
	}
	kr, err := trustFrom(*keyringPath, pubs)
	if err != nil {
		return err
	}
	trust := func(id string, created time.Time) (ed25519.PublicKey, error) {
		e, err := kr.KeyAt(id, created)
		if err != nil {
			return nil, err
		}
		return e.Public(), nil
	}
	var rep *bundle.Report
	if *extract != "" {
		rep, err = bundle.ExtractFile(*path, trust, *extract)
		if rep == nil {
			return err
		}
	} else {
		rep = bundle.VerifyFile(*path, trust)
	}
	if *format == "json" {
		if err := writeJSON(s.out, rep); err != nil {
			return err
		}
	} else {
		printBundleReport(s, *path, rep, kr, *extract)
	}
	if !rep.OK {
		return codedError{1, "bundle verification failed"}
	}
	return err
}

func printBundleReport(s streams, path string, rep *bundle.Report, kr *keyring.Keyring, extracted string) {
	status := "OK"
	if !rep.OK {
		status = "FAILED"
	}
	signer := rep.SignerKey
	if e, ok := kr.Lookup(rep.SignerKey); ok {
		signer = fmt.Sprintf("%s (%s, %s)", e.KeyID, e.Owner, e.Role)
	}
	var total int64
	for _, f := range rep.Files {
		total += f.Size
	}
	fmt.Fprintf(s.out, "bundle     %s\nstatus     %s\n", path, status)
	if rep.Manifest() != nil {
		fmt.Fprintf(s.out, "type       %s\nsubject    %s\ncreated    %s\nsigned by  %s\nbundle id  %s\n", rep.BundleType, rep.Subject, rep.Created, signer, rep.BundleID)
		if rep.Note != "" {
			fmt.Fprintf(s.out, "note       %s\n", rep.Note)
		}
		fmt.Fprintf(s.out, "files      %d (%d bytes)\n", len(rep.Files), total)
		tw := tabwriter.NewWriter(s.out, 0, 4, 2, ' ', 0)
		for _, f := range rep.Files {
			fmt.Fprintf(tw, "  %s\t%d\t%s\n", f.SHA256[:16], f.Size, f.Path)
		}
		tw.Flush()
	}
	if len(rep.Problems) > 0 {
		fmt.Fprintln(s.out, "problems")
		for _, p := range rep.Problems {
			fmt.Fprintf(s.out, "  %s\n", p)
		}
	}
	if rep.OK && extracted != "" {
		fmt.Fprintf(s.out, "extracted to %s\n", extracted)
	}
}
