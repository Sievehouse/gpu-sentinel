// Package bundle packs evidence files (an acceptance report and its raw
// results, a monthly statement with its inputs) into one signed .tar.gz that
// anyone holding the signer's public key can check offline.
//
// The archive's first entry is manifest.json, which lists every file with
// its SHA-256 and size and is signed with Ed25519. The files follow under
// files/. Verification checks the signature before reading anything else,
// then streams every file through SHA-256, so memory use does not grow with
// the bundle. It rejects files that are missing, extra, altered or unsafe to
// extract (absolute paths, "..", links, names that collide on
// case-insensitive file systems).
//
// The signature covers the manifest, not the compressed bytes, so a bundle
// stays valid if it is recompressed. With one build of the tool, the same
// inputs, time and key always give the same archive bytes.
package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/canonical"
	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
)

// Identifiers and layout.
const (
	APIVersion   = "capacity-ledger/v1alpha1"
	Kind         = "EvidenceBundle"
	ManifestName = "manifest.json"
	FilesDir     = "files/"
	signDomain   = "capacity-ledger/bundle/v1\n"
)

// Suggested bundle types. Any lowercase slug is accepted.
const (
	TypeAcceptance = "acceptance" // acceptance report, plan, raw results
	TypeStatement  = "statement"  // SLA statement with its contract, events and logs
	TypeIncident   = "incident"   // evidence about one incident or dispute
)

// Limits.
const (
	MaxFiles         = 10000
	MaxManifestBytes = 16 << 20
	MaxPathBytes     = 512
	maxProblems      = 100
)

// File is one entry of the manifest.
type File struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Signer identifies the signing key. The public key travels with the bundle
// for convenience; trust comes from the verifier's keyring, never from here.
type Signer struct {
	KeyID     string `json:"keyId"`
	PublicKey string `json:"publicKey"`
}

// Manifest describes and signs a bundle.
type Manifest struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	BundleType string `json:"bundleType"`
	Subject    string `json:"subject"`
	Created    string `json:"created"`
	Note       string `json:"note,omitempty"`
	Files      []File `json:"files"`
	Signer     Signer `json:"signer"`
	Signature  string `json:"signature"`
}

// Options describe a bundle being created.
type Options struct {
	Type    string    // for example TypeAcceptance
	Subject string    // what the evidence is about, such as a cluster or contract id
	Note    string    // optional free text
	Created time.Time // recorded to the second, in UTC
}

// Input is a file to put in a bundle. Open is called twice: once to hash the
// file for the manifest and once to copy it into the archive.
type Input struct {
	Path string // path inside the bundle, with forward slashes
	Open func() (io.ReadCloser, error)
}

// BytesInput returns an Input holding data.
func BytesInput(p string, data []byte) Input {
	return Input{Path: p, Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }}
}

// FileInput returns an Input that reads diskPath.
func FileInput(p, diskPath string) Input {
	return Input{Path: p, Open: func() (io.ReadCloser, error) { return os.Open(diskPath) }}
}

// DirInputs returns an Input for every regular file under dir, named
// prefix/<path relative to dir>. Symbolic links and other special files are
// an error, so a bundle never picks up something from outside dir.
func DirInputs(prefix, dir string) ([]Input, error) {
	var out []Input
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file (links and devices cannot be bundled)", p)
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if prefix != "" {
			name = prefix + "/" + name
		}
		if err := CheckPath(name); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, FileInput(name, p))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s contains no files", dir)
	}
	return out, nil
}

var (
	slugRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+@,~ -]*$`)
	hexRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	reserved  = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])$`)
)

// CheckPath reports whether p is acceptable as a path inside a bundle: one
// or more segments separated by "/", each starting with a letter, digit or
// underscore and using only letters, digits, spaces and ._+@,~- after that.
// Segments may not end in a dot or space or be a Windows device name. The
// rules make every bundle extractable on Linux, macOS and Windows.
func CheckPath(p string) error {
	if p == "" || len(p) > MaxPathBytes {
		return fmt.Errorf("bundle path %q must be 1 to %d bytes", p, MaxPathBytes)
	}
	for _, seg := range strings.Split(p, "/") {
		if len(seg) > 255 || !segmentRe.MatchString(seg) || strings.HasSuffix(seg, ".") || strings.HasSuffix(seg, " ") {
			return fmt.Errorf("bundle path %q: each part must start with a letter, digit or _ and use only letters, digits, spaces and ._+@,~- (no leading /, no .. and no backslashes)", p)
		}
		base, _, _ := strings.Cut(seg, ".")
		if reserved.MatchString(base) {
			return fmt.Errorf("bundle path %q: %q is a reserved device name on Windows", p, seg)
		}
	}
	return nil
}

// checkPathSet rejects sets of paths that would collide when extracted: two
// paths that differ only in case, or a path that is both a file and a
// directory. paths must already pass CheckPath.
func checkPathSet(paths []string) error {
	lower := make(map[string]string, len(paths))
	for _, p := range paths {
		l := strings.ToLower(p)
		if other, dup := lower[l]; dup {
			if other == p {
				return fmt.Errorf("duplicate bundle path %q", p)
			}
			return fmt.Errorf("bundle paths %q and %q differ only in case", other, p)
		}
		lower[l] = p
	}
	for _, p := range paths {
		l := strings.ToLower(p)
		for i := 0; i < len(l); i++ {
			if l[i] == '/' {
				if file, ok := lower[l[:i]]; ok {
					return fmt.Errorf("bundle path %q is a file, so %q cannot be inside it", file, p)
				}
			}
		}
	}
	return nil
}

// CheckSubject validates a bundle subject: 1 to 256 printable characters.
func CheckSubject(s string) error {
	if s == "" || len(s) > 256 {
		return fmt.Errorf("subject must be 1 to 256 bytes")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("subject %q contains a control character", s)
		}
	}
	return nil
}

func (m *Manifest) signingDigest() ([]byte, error) {
	cp := *m
	cp.Signature = ""
	b, err := canonical.Marshal(cp)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	h.Write([]byte(signDomain))
	h.Write(b)
	return h.Sum(nil), nil
}

// ID returns the SHA-256 of the canonical signed manifest. It names the
// bundle, for example in an evidence log "bundle" entry.
func (m *Manifest) ID() (string, error) { return canonical.SHA256Hex(m) }

// TotalBytes returns the sum of the file sizes.
func (m *Manifest) TotalBytes() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// LogRecord is the payload of an evidence log "bundle" entry. It commits the
// log to the bundle's exact contents without copying them.
type LogRecord struct {
	BundleID    string `json:"bundleId"`
	BundleType  string `json:"bundleType"`
	Subject     string `json:"subject"`
	Created     string `json:"created"`
	Files       int    `json:"files"`
	TotalBytes  int64  `json:"totalBytes"`
	SignerKeyID string `json:"signerKeyId"`
}

// LogRecord returns the evidence-log record for m.
func (m *Manifest) LogRecord() (LogRecord, error) {
	id, err := m.ID()
	if err != nil {
		return LogRecord{}, err
	}
	return LogRecord{
		BundleID: id, BundleType: m.BundleType, Subject: m.Subject, Created: m.Created,
		Files: len(m.Files), TotalBytes: m.TotalBytes(), SignerKeyID: m.Signer.KeyID,
	}, nil
}

func hashInput(in Input) (string, int64, error) {
	r, err := in.Open()
	if err != nil {
		return "", 0, err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, fmt.Errorf("%s: %w", in.Path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// Create writes a signed bundle of inputs to w and returns its manifest.
func Create(w io.Writer, opt Options, inputs []Input, priv ed25519.PrivateKey) (*Manifest, error) {
	if !slugRe.MatchString(opt.Type) {
		return nil, fmt.Errorf("bundle type %q must be a lowercase slug such as %q", opt.Type, TypeAcceptance)
	}
	if err := CheckSubject(opt.Subject); err != nil {
		return nil, err
	}
	if len(inputs) == 0 {
		return nil, errors.New("a bundle needs at least one file")
	}
	if len(inputs) > MaxFiles {
		return nil, fmt.Errorf("a bundle holds at most %d files", MaxFiles)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	pemText, err := evidence.PublicKeyPEM(pub)
	if err != nil {
		return nil, err
	}
	if opt.Created.IsZero() {
		return nil, errors.New("creation time is required")
	}
	created := opt.Created.UTC().Truncate(time.Second)
	sorted := append([]Input(nil), inputs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	paths := make([]string, len(sorted))
	for i, in := range sorted {
		if err := CheckPath(in.Path); err != nil {
			return nil, err
		}
		if in.Open == nil {
			return nil, fmt.Errorf("%s: no content", in.Path)
		}
		paths[i] = in.Path
	}
	if err := checkPathSet(paths); err != nil {
		return nil, err
	}

	m := &Manifest{
		APIVersion: APIVersion, Kind: Kind, BundleType: opt.Type, Subject: opt.Subject,
		Created: created.Format(time.RFC3339), Note: opt.Note, Files: make([]File, 0, len(sorted)),
		Signer: Signer{KeyID: evidence.KeyID(pub), PublicKey: pemText},
	}
	for _, in := range sorted {
		sum, size, err := hashInput(in)
		if err != nil {
			return nil, err
		}
		m.Files = append(m.Files, File{Path: in.Path, SHA256: sum, Size: size})
	}
	digest, err := m.signingDigest()
	if err != nil {
		return nil, err
	}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, digest))
	manifestJSON, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestJSON = append(manifestJSON, '\n')

	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	gz.ModTime = created
	tw := tar.NewWriter(gz)
	header := func(name string, size int64) *tar.Header {
		return &tar.Header{Name: name, Mode: 0o644, Size: size, ModTime: created, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	}
	if err := tw.WriteHeader(header(ManifestName, int64(len(manifestJSON)))); err != nil {
		return nil, err
	}
	if _, err := tw.Write(manifestJSON); err != nil {
		return nil, err
	}
	for i, in := range sorted {
		f := m.Files[i]
		if err := tw.WriteHeader(header(FilesDir+f.Path, f.Size)); err != nil {
			return nil, err
		}
		if err := copyChecked(tw, in, f); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return m, nil
}

// copyChecked copies an input into the archive and fails if it no longer
// matches what was hashed for the manifest.
func copyChecked(w io.Writer, in Input, f File) error {
	r, err := in.Open()
	if err != nil {
		return err
	}
	defer r.Close()
	h := sha256.New()
	n, err := io.Copy(w, io.TeeReader(io.LimitReader(r, f.Size), h))
	if err != nil {
		return fmt.Errorf("%s: %w", f.Path, err)
	}
	var probe [1]byte
	more, _ := r.Read(probe[:])
	if n != f.Size || more != 0 || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
		return fmt.Errorf("%s changed while the bundle was being written; try again", f.Path)
	}
	return nil
}

// Trust decides whether a signer may sign bundles and returns its public
// key. created is the bundle's creation time, for validity windows.
type Trust func(keyID string, created time.Time) (ed25519.PublicKey, error)

// TrustKeys trusts exactly the given keys, at any time.
func TrustKeys(keys map[string]ed25519.PublicKey) Trust {
	return func(id string, _ time.Time) (ed25519.PublicKey, error) {
		pub, ok := keys[id]
		if !ok {
			return nil, fmt.Errorf("signed by key %s, which is not trusted", id)
		}
		return pub, nil
	}
}

// Report is the result of verifying a bundle.
type Report struct {
	OK         bool     `json:"ok"`
	BundleID   string   `json:"bundleId,omitempty"`
	BundleType string   `json:"bundleType,omitempty"`
	Subject    string   `json:"subject,omitempty"`
	Created    string   `json:"created,omitempty"`
	Note       string   `json:"note,omitempty"`
	SignerKey  string   `json:"signerKeyId,omitempty"`
	Files      []File   `json:"files"`
	Problems   []string `json:"problems"`

	manifest *Manifest
}

// Manifest returns the manifest if it could be read, verified or not.
func (r *Report) Manifest() *Manifest { return r.manifest }

func (r *Report) fail(format string, a ...any) bool {
	r.Problems = append(r.Problems, fmt.Sprintf(format, a...))
	if len(r.Problems) >= maxProblems {
		r.Problems = append(r.Problems, "too many problems; stopped checking")
		return true
	}
	return false
}

// Verify reads a bundle, checks the manifest signature with trust and checks
// every file against the manifest.
func Verify(r io.Reader, trust Trust) *Report { return verify(r, trust, nil) }

// VerifyFile verifies the bundle at path.
func VerifyFile(p string, trust Trust) *Report {
	f, err := os.Open(p)
	if err != nil {
		return &Report{Files: []File{}, Problems: []string{err.Error()}}
	}
	defer f.Close()
	return Verify(f, trust)
}

// ExtractFile verifies the bundle at path and writes its files under dir,
// which must be empty or not exist yet. If the bundle does not verify,
// nothing is left behind.
func ExtractFile(p string, trust Trust, dir string) (*Report, error) {
	existed := false
	if entries, err := os.ReadDir(dir); err == nil {
		if len(entries) > 0 {
			return nil, fmt.Errorf("%s is not empty; extract into a new directory", dir)
		}
		existed = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cleanup := func() {
		if !existed {
			os.RemoveAll(dir)
			return
		}
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	f, err := os.Open(p)
	if err != nil {
		cleanup()
		return nil, err
	}
	defer f.Close()
	rep := verify(f, trust, &extractor{dir: dir})
	if !rep.OK {
		cleanup()
		return rep, errors.New("bundle did not verify; nothing was extracted")
	}
	return rep, nil
}

type extractor struct{ dir string }

func (x *extractor) create(p string) (*os.File, error) {
	dst := filepath.Join(x.dir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
}

func verify(r io.Reader, trust Trust, x *extractor) *Report {
	rep := &Report{Files: []File{}, Problems: []string{}}
	gz, err := gzip.NewReader(r)
	if err != nil {
		rep.fail("not a gzip file: %v", err)
		return rep
	}
	tr := tar.NewReader(gz)

	hdr, err := tr.Next()
	if err != nil {
		rep.fail("cannot read the archive: %v", err)
		return rep
	}
	if hdr.Name != ManifestName || hdr.Typeflag != tar.TypeReg {
		rep.fail("the first entry must be %s, found %q", ManifestName, hdr.Name)
		return rep
	}
	if hdr.Size > MaxManifestBytes {
		rep.fail("%s is larger than %d bytes", ManifestName, MaxManifestBytes)
		return rep
	}
	raw, err := io.ReadAll(tr)
	if err != nil {
		rep.fail("reading %s: %v", ManifestName, err)
		return rep
	}
	m, err := parseManifest(raw)
	if err != nil {
		rep.fail("%s: %v", ManifestName, err)
		return rep
	}
	rep.manifest = m
	rep.BundleType, rep.Subject, rep.Created, rep.Note, rep.SignerKey = m.BundleType, m.Subject, m.Created, m.Note, m.Signer.KeyID
	rep.Files = m.Files
	if id, err := m.ID(); err == nil {
		rep.BundleID = id
	}
	if err := checkSignature(m, trust); err != nil {
		rep.fail("%v", err)
		return rep
	}

	index := make(map[string]int, len(m.Files))
	for i, f := range m.Files {
		index[f.Path] = i
	}
	seen := make([]bool, len(m.Files))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			rep.fail("corrupt archive: %v", err)
			return rep
		}
		if hdr.Typeflag != tar.TypeReg {
			if rep.fail("entry %q is not a regular file", hdr.Name) {
				return rep
			}
			continue
		}
		p, under := strings.CutPrefix(hdr.Name, FilesDir)
		i, listed := index[p]
		switch {
		case !under || !listed:
			if rep.fail("%s: present but not listed in the manifest", hdr.Name) {
				return rep
			}
			continue
		case seen[i]:
			if rep.fail("%s: appears twice", p) {
				return rep
			}
			continue
		}
		seen[i] = true
		f := m.Files[i]
		if hdr.Size != f.Size {
			if rep.fail("%s: %d bytes, manifest says %d", p, hdr.Size, f.Size) {
				return rep
			}
			continue
		}
		h := sha256.New()
		var dst io.Writer = h
		var out *os.File
		if x != nil {
			if out, err = x.create(p); err != nil {
				rep.fail("%s: cannot extract: %v", p, err)
				return rep
			}
			dst = io.MultiWriter(h, out)
		}
		n, err := io.Copy(dst, tr)
		if out != nil {
			if cerr := out.Close(); err == nil {
				err = cerr
			}
		}
		if err != nil {
			rep.fail("%s: %v", p, err)
			return rep
		}
		if n != f.Size || hex.EncodeToString(h.Sum(nil)) != f.SHA256 {
			if rep.fail("%s: content does not match the manifest", p) {
				return rep
			}
		}
	}
	for i, f := range m.Files {
		if !seen[i] {
			if rep.fail("%s: listed in the manifest but missing", f.Path) {
				return rep
			}
		}
	}
	rep.OK = len(rep.Problems) == 0
	return rep
}

func parseManifest(raw []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the manifest")
	}
	if m.APIVersion != APIVersion || m.Kind != Kind {
		return nil, fmt.Errorf("not a %s %s", APIVersion, Kind)
	}
	if !slugRe.MatchString(m.BundleType) {
		return nil, fmt.Errorf("bad bundle type %q", m.BundleType)
	}
	if err := CheckSubject(m.Subject); err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.RFC3339, m.Created); err != nil {
		return nil, fmt.Errorf("bad created time %q", m.Created)
	}
	if len(m.Files) == 0 || len(m.Files) > MaxFiles {
		return nil, fmt.Errorf("a bundle lists 1 to %d files, this one lists %d", MaxFiles, len(m.Files))
	}
	paths := make([]string, len(m.Files))
	for i, f := range m.Files {
		if err := CheckPath(f.Path); err != nil {
			return nil, err
		}
		if i > 0 && f.Path <= m.Files[i-1].Path {
			return nil, fmt.Errorf("files are not sorted by path at %q", f.Path)
		}
		if !hexRe.MatchString(f.SHA256) || f.Size < 0 {
			return nil, fmt.Errorf("%s: bad sha256 or size", f.Path)
		}
		paths[i] = f.Path
	}
	if err := checkPathSet(paths); err != nil {
		return nil, err
	}
	return &m, nil
}

func checkSignature(m *Manifest, trust Trust) error {
	created, err := time.Parse(time.RFC3339, m.Created)
	if err != nil {
		return err
	}
	pub, err := trust(m.Signer.KeyID, created)
	if err != nil {
		return err
	}
	embedded, err := evidence.ParsePublicKeyPEM(m.Signer.PublicKey)
	if err != nil || !embedded.Equal(pub) || evidence.KeyID(pub) != m.Signer.KeyID {
		return fmt.Errorf("the manifest's public key does not match trusted key %s", m.Signer.KeyID)
	}
	digest, err := m.signingDigest()
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || !ed25519.Verify(pub, digest, sig) {
		return errors.New("invalid manifest signature: the manifest was changed after signing or signed by another key")
	}
	return nil
}
