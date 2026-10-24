package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
)

var created = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func key(seed byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, ed25519.SeedSize))
}

func trust(privs ...ed25519.PrivateKey) Trust {
	keys := map[string]ed25519.PublicKey{}
	for _, p := range privs {
		pub := p.Public().(ed25519.PublicKey)
		keys[idOf(pub)] = pub
	}
	return TrustKeys(keys)
}

func idOf(pub ed25519.PublicKey) string { return evidence.KeyID(pub) }

func pemOf(pub ed25519.PublicKey) string {
	s, err := evidence.PublicKeyPEM(pub)
	if err != nil {
		panic(err)
	}
	return s
}

func sample() []Input {
	return []Input{
		BytesInput("report.md", []byte("# Acceptance: example-hgx-32gpu\n\n4 accept, 0 review, 0 reject\n")),
		BytesInput("report.json", []byte(`{"summary":{"accept":4,"review":0,"reject":0}}`+"\n")),
		BytesInput("raw/gpu-001/dcgm-diag.txt", []byte("Diagnostic | Result\nMemory     | Pass\n")),
	}
}

func build(t *testing.T, inputs []Input) ([]byte, *Manifest) {
	t.Helper()
	var buf bytes.Buffer
	m, err := Create(&buf, Options{Type: TypeAcceptance, Subject: "example-hgx-32gpu", Note: "test", Created: created}, inputs, key(1))
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), m
}

type entry struct {
	hdr  tar.Header
	data []byte
}

// repack reads every entry of a bundle, lets edit change them and writes
// them back without re-signing.
func repack(t *testing.T, b []byte, edit func([]entry) []entry) []byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var es []entry
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		es = append(es, entry{*hdr, data})
	}
	es = edit(es)
	var out bytes.Buffer
	gw := gzip.NewWriter(&out)
	tw := tar.NewWriter(gw)
	for _, e := range es {
		h := e.hdr
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gw.Close()
	return out.Bytes()
}

func problems(rep *Report) string { return strings.Join(rep.Problems, "\n") }

func wantProblem(t *testing.T, rep *Report, want string) {
	t.Helper()
	if rep.OK {
		t.Fatalf("bundle verified; want a problem containing %q", want)
	}
	if !strings.Contains(problems(rep), want) {
		t.Fatalf("problems:\n%s\nwant one containing %q", problems(rep), want)
	}
}

func TestRoundTrip(t *testing.T) {
	b, m := build(t, sample())
	rep := Verify(bytes.NewReader(b), trust(key(1)))
	if !rep.OK {
		t.Fatalf("problems: %s", problems(rep))
	}
	id, _ := m.ID()
	if rep.BundleID != id || rep.BundleType != TypeAcceptance || rep.Subject != "example-hgx-32gpu" || rep.Created != "2026-10-07T12:00:00Z" || rep.Note != "test" {
		t.Fatalf("report = %+v", rep)
	}
	var paths []string
	for _, f := range rep.Files {
		paths = append(paths, f.Path)
	}
	if got := strings.Join(paths, ","); got != "raw/gpu-001/dcgm-diag.txt,report.json,report.md" {
		t.Fatalf("files = %s", got)
	}

	dir := filepath.Join(t.TempDir(), "out")
	p := filepath.Join(t.TempDir(), "b.tar.gz")
	os.WriteFile(p, b, 0o644)
	if _, err := ExtractFile(p, trust(key(1)), dir); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "raw", "gpu-001", "dcgm-diag.txt"))
	if err != nil || string(got) != "Diagnostic | Result\nMemory     | Pass\n" {
		t.Fatalf("extracted %q, %v", got, err)
	}

	rec, err := m.LogRecord()
	if err != nil || rec.BundleID != id || rec.Files != 3 || rec.TotalBytes != m.TotalBytes() || rec.SignerKeyID != m.Signer.KeyID {
		t.Fatalf("log record = %+v, %v", rec, err)
	}
}

func TestDeterministic(t *testing.T) {
	a, _ := build(t, sample())
	b, _ := build(t, sample())
	if !bytes.Equal(a, b) {
		t.Fatal("same inputs gave different bundles")
	}
	var c bytes.Buffer
	if _, err := Create(&c, Options{Type: TypeAcceptance, Subject: "example-hgx-32gpu", Note: "test", Created: created.Add(time.Second)}, sample(), key(1)); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, c.Bytes()) {
		t.Fatal("a different creation time gave the same bundle")
	}
}

func TestRecompressedBundleStillVerifies(t *testing.T) {
	b, _ := build(t, sample())
	re := repack(t, b, func(es []entry) []entry { return es })
	if bytes.Equal(b, re) {
		t.Fatal("expected different compressed bytes")
	}
	if rep := Verify(bytes.NewReader(re), trust(key(1))); !rep.OK {
		t.Fatalf("problems: %s", problems(rep))
	}
}

func TestUntrustedOrWrongKey(t *testing.T) {
	b, _ := build(t, sample())
	wantProblem(t, Verify(bytes.NewReader(b), trust(key(2))), "not trusted")

	// A manifest that claims a trusted key id but embeds another key.
	other := key(2).Public().(ed25519.PublicKey)
	re := repack(t, b, func(es []entry) []entry {
		var m Manifest
		json.Unmarshal(es[0].data, &m)
		m.Signer.PublicKey = pemOf(other)
		es[0].data, _ = json.Marshal(m)
		return es
	})
	wantProblem(t, Verify(bytes.NewReader(re), trust(key(1))), "does not match trusted key")
}

func TestEditedManifestFailsSignature(t *testing.T) {
	b, _ := build(t, sample())
	re := repack(t, b, func(es []entry) []entry {
		var m Manifest
		json.Unmarshal(es[0].data, &m)
		m.Subject = "another-cluster"
		es[0].data, _ = json.Marshal(m)
		return es
	})
	rep := Verify(bytes.NewReader(re), trust(key(1)))
	wantProblem(t, rep, "invalid manifest signature")
	if rep.Subject != "another-cluster" || rep.Manifest() == nil {
		t.Fatalf("the report should still describe the unverified manifest: %+v", rep)
	}
}

func TestTamperedFiles(t *testing.T) {
	b, _ := build(t, sample())
	edit := func(name string, f func(entry) entry) []byte {
		return repack(t, b, func(es []entry) []entry {
			for i := range es {
				if es[i].hdr.Name == name {
					es[i] = f(es[i])
				}
			}
			return es
		})
	}
	sameSize := edit("files/report.md", func(e entry) entry {
		e.data = bytes.Replace(e.data, []byte("4 accept"), []byte("3 accept"), 1)
		return e
	})
	wantProblem(t, Verify(bytes.NewReader(sameSize), trust(key(1))), "report.md: content does not match the manifest")

	longer := edit("files/report.json", func(e entry) entry { e.data = append(e.data, ' '); return e })
	wantProblem(t, Verify(bytes.NewReader(longer), trust(key(1))), "report.json: 48 bytes, manifest says 47")
}

func TestMissingExtraAndDuplicateEntries(t *testing.T) {
	b, _ := build(t, sample())
	missing := repack(t, b, func(es []entry) []entry { return es[:len(es)-1] })
	wantProblem(t, Verify(bytes.NewReader(missing), trust(key(1))), "report.md: listed in the manifest but missing")

	add := func(e entry) []byte { return repack(t, b, func(es []entry) []entry { return append(es, e) }) }
	reg := func(name string) entry {
		return entry{hdr: tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg, ModTime: created}, data: []byte("x")}
	}
	wantProblem(t, Verify(bytes.NewReader(add(reg("files/extra.txt"))), trust(key(1))), "files/extra.txt: present but not listed")
	wantProblem(t, Verify(bytes.NewReader(add(reg("files/../../etc/passwd"))), trust(key(1))), "present but not listed")
	wantProblem(t, Verify(bytes.NewReader(add(reg("README"))), trust(key(1))), "README: present but not listed")
	wantProblem(t, Verify(bytes.NewReader(add(entry{hdr: tar.Header{Name: "files/link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", ModTime: created}})), trust(key(1))), `entry "files/link" is not a regular file`)

	dup := repack(t, b, func(es []entry) []entry { return append(es, es[1]) })
	wantProblem(t, Verify(bytes.NewReader(dup), trust(key(1))), "appears twice")
}

func TestManifestMustComeFirst(t *testing.T) {
	b, _ := build(t, sample())
	re := repack(t, b, func(es []entry) []entry { return append(es[1:], es[0]) })
	wantProblem(t, Verify(bytes.NewReader(re), trust(key(1))), "the first entry must be manifest.json")
}

func TestCorruptInput(t *testing.T) {
	b, _ := build(t, sample())
	wantProblem(t, Verify(strings.NewReader("not a bundle"), trust(key(1))), "not a gzip file")
	rep := Verify(bytes.NewReader(b[:len(b)*2/3]), trust(key(1)))
	if rep.OK {
		t.Fatal("truncated bundle verified")
	}
}

func TestSignedButMalformedManifest(t *testing.T) {
	priv := key(1)
	sign := func(m *Manifest) []byte {
		m.Signer = Signer{KeyID: idOf(priv.Public().(ed25519.PublicKey)), PublicKey: pemOf(priv.Public().(ed25519.PublicKey))}
		d, _ := m.signingDigest()
		m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, d))
		data, _ := json.Marshal(m)
		var out bytes.Buffer
		gw := gzip.NewWriter(&out)
		tw := tar.NewWriter(gw)
		tw.WriteHeader(&tar.Header{Name: ManifestName, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg})
		tw.Write(data)
		tw.Close()
		gw.Close()
		return out.Bytes()
	}
	base := func() *Manifest {
		return &Manifest{APIVersion: APIVersion, Kind: Kind, BundleType: "acceptance", Subject: "s", Created: "2026-10-07T12:00:00Z"}
	}
	h := strings.Repeat("a", 64)
	cases := map[string]func(m *Manifest){
		"not sorted by path":    func(m *Manifest) { m.Files = []File{{"b", h, 1}, {"a", h, 1}} },
		"differ only in case":   func(m *Manifest) { m.Files = []File{{"A", h, 1}, {"a", h, 1}} },
		"cannot be inside it":   func(m *Manifest) { m.Files = []File{{"a", h, 1}, {"a/b", h, 1}} },
		"each part must start":  func(m *Manifest) { m.Files = []File{{"../x", h, 1}} },
		"bad sha256 or size":    func(m *Manifest) { m.Files = []File{{"a", "zz", 1}} },
		"lists 1 to":            func(m *Manifest) { m.Files = nil },
		"bad bundle type":       func(m *Manifest) { m.BundleType = "Acceptance"; m.Files = []File{{"a", h, 1}} },
		"bad created time":      func(m *Manifest) { m.Created = "yesterday"; m.Files = []File{{"a", h, 1}} },
		"reserved device name":  func(m *Manifest) { m.Files = []File{{"logs/aux.txt", h, 1}} },
		"contains a control ch": func(m *Manifest) { m.Subject = "a\nb"; m.Files = []File{{"a", h, 1}} },
	}
	for want, mutate := range cases {
		m := base()
		mutate(m)
		wantProblem(t, Verify(bytes.NewReader(sign(m)), trust(priv)), want)
	}
}

func TestCheckPath(t *testing.T) {
	good := []string{"report.json", "raw/gpu-001/dcgm-diag.txt", "a", "_x", "Acceptance Report v2.md", "nccl+ib@4,node~1.txt", "console.log", "auxiliary.txt"}
	bad := []string{"", "/etc/passwd", "../x", "a/../b", "a//b", "./a", "a/", ".hidden", "a\\b", "C:/x", "c:x", "a/..", "x.", "x ", " x", "con", "NUL.txt", "lpt1", "a/com3.log", "a\x00b", "tab\tname", "semi;colon", "quote\"", "a=b", strings.Repeat("a", 256), strings.Repeat("a/", 300) + "a"}
	for _, p := range good {
		if err := CheckPath(p); err != nil {
			t.Errorf("CheckPath(%q) = %v", p, err)
		}
	}
	for _, p := range bad {
		if CheckPath(p) == nil {
			t.Errorf("CheckPath(%q) accepted", p)
		}
	}
}

func TestCreateRejects(t *testing.T) {
	opt := Options{Type: TypeAcceptance, Subject: "s", Created: created}
	one := []Input{BytesInput("a", []byte("x"))}
	cases := []struct {
		name   string
		opt    Options
		inputs []Input
		want   string
	}{
		{"type", Options{Type: "Bad Type", Subject: "s", Created: created}, one, "lowercase slug"},
		{"subject", Options{Type: "acceptance", Created: created}, one, "subject must be"},
		{"time", Options{Type: "acceptance", Subject: "s"}, one, "creation time"},
		{"empty", opt, nil, "at least one file"},
		{"path", opt, []Input{BytesInput("../a", nil)}, "each part must start"},
		{"duplicate", opt, []Input{BytesInput("a", nil), BytesInput("a", nil)}, "duplicate bundle path"},
		{"case", opt, []Input{BytesInput("Report.md", nil), BytesInput("report.md", nil)}, "differ only in case"},
		{"file and dir", opt, []Input{BytesInput("raw", nil), BytesInput("raw/x", nil)}, "cannot be inside it"},
	}
	for _, c := range cases {
		_, err := Create(io.Discard, c.opt, c.inputs, key(1))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

func TestInputChangedWhileWriting(t *testing.T) {
	calls := 0
	in := Input{Path: "growing.log", Open: func() (io.ReadCloser, error) {
		calls++
		return io.NopCloser(strings.NewReader(strings.Repeat("x", calls))), nil
	}}
	_, err := Create(io.Discard, Options{Type: "acceptance", Subject: "s", Created: created}, []Input{in}, key(1))
	if err == nil || !strings.Contains(err.Error(), "changed while the bundle was being written") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractSafety(t *testing.T) {
	b, _ := build(t, sample())
	tmp := t.TempDir()
	good := filepath.Join(tmp, "good.tar.gz")
	os.WriteFile(good, b, 0o644)

	full := filepath.Join(tmp, "full")
	os.MkdirAll(full, 0o755)
	os.WriteFile(filepath.Join(full, "keep.txt"), []byte("mine"), 0o644)
	if _, err := ExtractFile(good, trust(key(1)), full); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("err = %v", err)
	}

	bad := filepath.Join(tmp, "bad.tar.gz")
	os.WriteFile(bad, repack(t, b, func(es []entry) []entry { return es[:len(es)-1] }), 0o644)
	fresh := filepath.Join(tmp, "fresh")
	if _, err := ExtractFile(bad, trust(key(1)), fresh); err == nil {
		t.Fatal("extracted a bundle with a missing file")
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("a failed extraction left %s behind (%v)", fresh, err)
	}
	empty := filepath.Join(tmp, "empty")
	os.MkdirAll(empty, 0o755)
	if _, err := ExtractFile(bad, trust(key(1)), empty); err == nil {
		t.Fatal("extracted a bundle with a missing file")
	}
	if left, _ := os.ReadDir(empty); len(left) != 0 {
		t.Fatalf("a failed extraction left %d entries in a pre-existing directory", len(left))
	}
}

func TestDirInputs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "raw")
	os.MkdirAll(filepath.Join(dir, "gpu-001"), 0o755)
	os.WriteFile(filepath.Join(dir, "gpu-001", "dcgm.txt"), []byte("PASS\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "summary.txt"), []byte("ok\n"), 0o644)
	ins, err := DirInputs("raw", dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, in := range ins {
		names = append(names, in.Path)
	}
	if got := strings.Join(names, ","); got != "raw/gpu-001/dcgm.txt,raw/summary.txt" {
		t.Fatalf("names = %s", got)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "passwd")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if _, err := DirInputs("raw", dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v", err)
	}
}
