package ledger

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
	"github.com/Sievehouse/gpu-sentinel/internal/sla"
)

// Limits on request bodies.
const (
	MaxPushBytes     = 64 << 20
	MaxDocumentBytes = 16 << 20
)

// FeedKind identifies the financier feed document.
const FeedKind = "LedgerFeed"

// Server is the ledger's HTTP API. Create it with NewServer and serve
// Handler().
type Server struct {
	store   *Store
	witness ed25519.PrivateKey
	state   atomic.Pointer[state]
	logw    io.Writer
	logMu   sync.Mutex
	now     func() time.Time
	// witnessName names the witness in checkpoints; prefix is prepended to
	// logged paths. A Hub sets both to tell its contracts apart.
	witnessName string
	prefix      string
	muxOnce     sync.Once
	mux         http.Handler
}

// handler returns the HTTP API, built once.
func (s *Server) handler() http.Handler {
	s.muxOnce.Do(func() { s.mux = s.Handler() })
	return s.mux
}

type state struct {
	cfg *Config
	kr  *keyring.Keyring
}

// NewServer returns a server over store. witness signs the checkpoints;
// logw receives one line per request (nil to discard).
func NewServer(store *Store, cfg *Config, kr *keyring.Keyring, witness ed25519.PrivateKey, logw io.Writer) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if logw == nil {
		logw = io.Discard
	}
	s := &Server{store: store, witness: witness, logw: logw, now: time.Now, witnessName: "ledger witness"}
	s.state.Store(&state{cfg: cfg, kr: kr})
	return s, nil
}

// Reload swaps in a new configuration and keyring, for example after an
// operator enrolled a key. The contract may not change.
func (s *Server) Reload(cfg *Config, kr *keyring.Keyring) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if old := s.state.Load(); old.cfg.Contract != cfg.Contract {
		return fmt.Errorf("the ledger serves contract %q; it cannot switch to %q", old.cfg.Contract, cfg.Contract)
	}
	s.state.Store(&state{cfg: cfg, kr: kr})
	return nil
}

// SetClock replaces the server's clock (for tests).
func (s *Server) SetClock(now func() time.Time) { s.now = now }

// WitnessKeyID returns the id of the key that signs checkpoints.
func (s *Server) WitnessKeyID() string {
	return evidence.KeyID(s.witness.Public().(ed25519.PublicKey))
}

type httpError struct {
	code int
	msg  string
	data map[string]any
}

func (e *httpError) Error() string { return e.msg }

func fail(code int, format string, a ...any) *httpError {
	return &httpError{code: code, msg: fmt.Sprintf(format, a...)}
}

type handler func(w http.ResponseWriter, r *http.Request, p *Party, st *state) error

// Handler returns the HTTP API.
func (s *Server) Handler() http.Handler {
	all := []string{RoleProvider, RoleCustomer, RoleFinancier, RoleAuditor}
	parties := []string{RoleProvider, RoleCustomer}
	readers := []string{RoleProvider, RoleCustomer, RoleAuditor}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	route := func(pattern string, roles []string, h handler) { mux.Handle(pattern, s.auth(roles, h)) }
	route("GET /v1/info", all, s.info)
	route("GET /v1/logs", readers, s.listLogs)
	route("GET /v1/logs/{name}", readers, s.readLog)
	route("GET /v1/logs/{name}/head", readers, s.logHead)
	route("POST /v1/logs/{name}", parties, s.pushLog)
	route("POST /v1/checkpoints", readers, s.createCheckpoint)
	route("GET /v1/checkpoints", readers, s.listCheckpoints)
	route("GET /v1/checkpoints/latest", readers, s.latestCheckpoint)
	route("GET /v1/checkpoints/{id}", readers, s.getCheckpoint)
	route("PUT /v1/statements", parties, s.putStatement)
	route("GET /v1/statements", readers, s.listStatements)
	route("GET /v1/statements/{sha}", readers, s.getStatement)
	route("GET /v1/statements/{sha}/signatures", readers, s.getSignatures)
	route("POST /v1/statements/{sha}/signatures", all, s.postSignature)
	route("POST /v1/consents", parties, s.postConsent)
	route("GET /v1/consents", all, s.listConsents)
	route("GET /v1/feed", all, s.feed)
	return mux
}

// auth resolves the bearer token to a party, checks its role, runs h and
// logs the request.
func (s *Server) auth(roles []string, h handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		rec := &recorder{ResponseWriter: w}
		w = rec
		st := s.state.Load()
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, ok := st.cfg.ByToken(strings.TrimSpace(token))
		who := "-"
		var err error
		switch {
		case !ok:
			err = fail(http.StatusUnauthorized, "missing or unknown bearer token")
		case !contains(roles, p.Role):
			who = p.Name
			err = fail(http.StatusForbidden, "a %s may not %s %s", p.Role, r.Method, r.URL.Path)
		default:
			who = p.Name
			err = h(w, r, p, st)
		}
		if err != nil && !rec.wrote {
			writeError(w, err)
		}
		code := rec.code
		if code == 0 {
			code = http.StatusOK
		}
		s.logMu.Lock()
		fmt.Fprintf(s.logw, "%s %s %s %s %d %s\n", start.UTC().Format(time.RFC3339), who, r.Method, s.prefix+r.URL.Path, code, s.now().Sub(start).Round(time.Millisecond))
		s.logMu.Unlock()
	})
}

// recorder remembers the status code and whether a response was started.
type recorder struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.code, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wrote {
		r.code, r.wrote = http.StatusOK, true
	}
	return r.ResponseWriter.Write(b)
}

func writeError(w http.ResponseWriter, err error) int {
	var he *httpError
	body := map[string]any{"error": err.Error()}
	code := http.StatusInternalServerError
	switch {
	case errors.As(err, &he):
		code = he.code
		for k, v := range he.data {
			body[k] = v
		}
	case errors.Is(err, ErrNotFound):
		code = http.StatusNotFound
	}
	writeJSON(w, code, body)
	return code
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		return nil, fail(http.StatusBadRequest, "reading the request: %v", err)
	}
	if int64(len(b)) > limit {
		return nil, fail(http.StatusRequestEntityTooLarge, "request larger than %d bytes; push in smaller parts", limit)
	}
	return b, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Info describes the ledger to the calling party.
type Info struct {
	Contract string `json:"contract"`
	Party    struct {
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"party"`
	Witness struct {
		KeyID     string `json:"keyId"`
		PublicKey string `json:"publicKey"`
	} `json:"witness"`
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request, p *Party, st *state) error {
	var in Info
	in.Contract = st.cfg.Contract
	in.Party.Name, in.Party.Role = p.Name, p.Role
	pemText, err := evidence.PublicKeyPEM(s.witness.Public().(ed25519.PublicKey))
	if err != nil {
		return err
	}
	in.Witness.KeyID, in.Witness.PublicKey = s.WitnessKeyID(), pemText
	writeJSON(w, http.StatusOK, in)
	return nil
}

func (s *Server) listLogs(w http.ResponseWriter, _ *http.Request, _ *Party, _ *state) error {
	writeJSON(w, http.StatusOK, s.store.Logs())
	return nil
}

func (s *Server) logHead(w http.ResponseWriter, r *http.Request, _ *Party, _ *state) error {
	info, ok := s.store.Log(r.PathValue("name"))
	if !ok {
		return ErrNotFound
	}
	writeJSON(w, http.StatusOK, info)
	return nil
}

func (s *Server) readLog(w http.ResponseWriter, r *http.Request, _ *Party, _ *state) error {
	name := r.PathValue("name")
	var from int64
	if v := r.URL.Query().Get("from"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return fail(http.StatusBadRequest, "from must be a number of entries to skip")
		}
		from = n
	}
	if _, ok := s.store.Log(name); !ok {
		return ErrNotFound
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	return s.store.CopyLog(name, from, w)
}

// pushLog appends entries to a log. They must continue the stored chain,
// carry valid signatures from keys in the keyring that the pushing party may
// write for, and pass the keyring's validity and node-binding rules. A log
// belongs to the party that pushed it first.
func (s *Server) pushLog(w http.ResponseWriter, r *http.Request, p *Party, st *state) error {
	name := r.PathValue("name")
	body, err := readBody(r, MaxPushBytes)
	if err != nil {
		return err
	}
	entries, err := evidence.ReadEntries(bytes.NewReader(body))
	if err != nil {
		return fail(http.StatusBadRequest, "entries: %v", err)
	}
	if len(entries) == 0 {
		return fail(http.StatusBadRequest, "no entries in the request")
	}
	check := st.kr.EntryCheck()
	info, err := s.store.Append(name, p.Name, entries, func(cur LogInfo, exists bool) error {
		if exists && cur.Owner != p.Name {
			return fail(http.StatusForbidden, "log %s belongs to %s", name, cur.Owner)
		}
		prev, expect := cur.LastHash, cur.Entries+1
		if cur.Entries == 0 {
			prev = evidence.GenesisPrev
		}
		for _, e := range entries {
			if e.Seq != expect || e.Prev != prev {
				he := fail(http.StatusConflict, "seq %d does not continue log %s, which holds %d entries ending in %s", e.Seq, name, cur.Entries, cur.LastHash)
				he.data = map[string]any{"head": cur}
				return he
			}
			if err := verifyEntry(e, st.kr, check, p); err != nil {
				return fail(http.StatusBadRequest, "seq %d: %v", e.Seq, err)
			}
			prev, expect = e.Hash, e.Seq+1
		}
		return nil
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, info)
	return nil
}

func verifyEntry(e evidence.Entry, kr *keyring.Keyring, check evidence.EntryCheck, p *Party) error {
	h, err := e.ComputeHash()
	if err != nil || h != e.Hash {
		return errors.New("content does not match its hash")
	}
	k, ok := kr.Lookup(e.KeyID)
	if !ok {
		return fmt.Errorf("signed by key %s, which is not in the ledger's keyring", e.KeyID)
	}
	digest, _ := hex.DecodeString(e.Hash)
	sig, err := base64.StdEncoding.DecodeString(e.Sig)
	if err != nil || !ed25519.Verify(k.Public(), digest, sig) {
		return errors.New("invalid signature")
	}
	if err := check(e); err != nil {
		return err
	}
	if !p.MayWrite(k.Role) {
		return fmt.Errorf("signed by a %s key (%s); %s may push only %s logs", k.Role, k.Owner, p.Name, strings.Join(p.WriteRoles, ", "))
	}
	return nil
}

// CheckpointResult is what the checkpoint endpoints return.
type CheckpointResult struct {
	ID         string               `json:"id"`
	Checkpoint *evidence.Checkpoint `json:"checkpoint"`
}

// Checkpoint signs the current head of every log as the ledger's witness.
// It returns nil if the ledger holds no logs, or if force is false and no
// log changed since the latest checkpoint.
func (s *Server) Checkpoint(force bool) (*CheckpointResult, error) {
	heads := s.store.Heads()
	if len(heads) == 0 {
		return nil, nil
	}
	if !force {
		ids, err := s.store.Checkpoints()
		if err != nil {
			return nil, err
		}
		if len(ids) > 0 {
			last, err := s.store.Checkpoint(ids[len(ids)-1])
			if err != nil {
				return nil, err
			}
			if sameHeads(last.Logs, heads) {
				return nil, nil
			}
		}
	}
	cp, err := evidence.NewCheckpoint(heads, s.witnessName, s.witness, s.now())
	if err != nil {
		return nil, err
	}
	id, err := s.store.SaveCheckpoint(cp)
	if err != nil {
		return nil, err
	}
	return &CheckpointResult{ID: id, Checkpoint: cp}, nil
}

func sameHeads(a, b []evidence.LogHead) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Server) createCheckpoint(w http.ResponseWriter, _ *http.Request, _ *Party, _ *state) error {
	res, err := s.Checkpoint(true)
	if err != nil {
		return err
	}
	if res == nil {
		return fail(http.StatusConflict, "the ledger holds no logs yet")
	}
	writeJSON(w, http.StatusCreated, res)
	return nil
}

func (s *Server) listCheckpoints(w http.ResponseWriter, _ *http.Request, _ *Party, _ *state) error {
	ids, err := s.store.Checkpoints()
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ids)
	return nil
}

func (s *Server) latestCheckpoint(w http.ResponseWriter, _ *http.Request, _ *Party, _ *state) error {
	ids, err := s.store.Checkpoints()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return ErrNotFound
	}
	return s.sendCheckpoint(w, ids[len(ids)-1])
}

func (s *Server) getCheckpoint(w http.ResponseWriter, r *http.Request, _ *Party, _ *state) error {
	return s.sendCheckpoint(w, r.PathValue("id"))
}

func (s *Server) sendCheckpoint(w http.ResponseWriter, id string) error {
	cp, err := s.store.Checkpoint(id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, CheckpointResult{ID: id, Checkpoint: cp})
	return nil
}

func (s *Server) putStatement(w http.ResponseWriter, r *http.Request, _ *Party, st *state) error {
	body, err := readBody(r, MaxDocumentBytes)
	if err != nil {
		return err
	}
	stmt, err := sla.DecodeStatement(body)
	if err != nil {
		return fail(http.StatusBadRequest, "statement: %v", err)
	}
	if err := stmt.CheckSeal(); err != nil {
		return fail(http.StatusBadRequest, "%v", err)
	}
	if stmt.Contract.ID != st.cfg.Contract {
		return fail(http.StatusBadRequest, "statement is for contract %q; this ledger serves %q", stmt.Contract.ID, st.cfg.Contract)
	}
	created, err := s.store.PutStatement(stmt)
	if err != nil {
		return err
	}
	code := http.StatusOK
	if created {
		code = http.StatusCreated
	}
	writeJSON(w, code, map[string]any{"statementSha256": stmt.StatementSHA256, "created": created})
	return nil
}

// StatementSummary is one line of the statement list.
type StatementSummary struct {
	StatementSHA256     string `json:"statementSha256"`
	ContractVersion     int    `json:"contractVersion"`
	PeriodStart         string `json:"periodStart"`
	PeriodEnd           string `json:"periodEnd"`
	AvailabilityPercent string `json:"availabilityPercent"`
	CreditPercent       int    `json:"creditPercent"`
	CreditAmount        string `json:"creditAmount,omitempty"`
	EngineVersion       string `json:"engineVersion"`
}

func summarize(st *sla.Statement) StatementSummary {
	return StatementSummary{
		StatementSHA256: st.StatementSHA256, ContractVersion: st.Contract.Version, PeriodStart: st.Period.Start, PeriodEnd: st.Period.End,
		AvailabilityPercent: st.Summary.AvailabilityPercent, CreditPercent: st.Credit.Percent, CreditAmount: st.Credit.Amount,
		EngineVersion: st.EngineVersion,
	}
}

func (s *Server) listStatements(w http.ResponseWriter, _ *http.Request, _ *Party, _ *state) error {
	sts, err := s.store.Statements()
	if err != nil {
		return err
	}
	out := make([]StatementSummary, 0, len(sts))
	for _, st := range sts {
		out = append(out, summarize(st))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) getStatement(w http.ResponseWriter, r *http.Request, _ *Party, _ *state) error {
	st, err := s.store.Statement(r.PathValue("sha"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

// RequiredParties must all accept a statement for it to be co-signed.
var RequiredParties = []string{RoleProvider, RoleCustomer}

// SignatureSet is a statement's signatures and their co-signing status.
type SignatureSet struct {
	Signatures []*sla.Signature `json:"signatures"`
	CoSign     sla.CoSignReport `json:"coSign"`
}

func (s *Server) signatureSet(sha string, kr *keyring.Keyring) (*sla.Statement, *SignatureSet, error) {
	st, err := s.store.Statement(sha)
	if err != nil {
		return nil, nil, err
	}
	sigs, err := s.store.Signatures(sha)
	if err != nil {
		return nil, nil, err
	}
	if sigs == nil {
		sigs = []*sla.Signature{}
	}
	return st, &SignatureSet{Signatures: sigs, CoSign: sla.CheckCoSigned(st, sigs, nil, kr.SignerAt, RequiredParties)}, nil
}

func (s *Server) getSignatures(w http.ResponseWriter, r *http.Request, _ *Party, st *state) error {
	_, set, err := s.signatureSet(r.PathValue("sha"), st.kr)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, set)
	return nil
}

// postSignature stores a party's signature on a statement. The token's
// party must be the role the signature claims, and the signature must
// verify with a key of that role in the keyring. A financier may sign only
// while its feed is open.
func (s *Server) postSignature(w http.ResponseWriter, r *http.Request, p *Party, st *state) error {
	sha := r.PathValue("sha")
	body, err := readBody(r, MaxDocumentBytes)
	if err != nil {
		return err
	}
	sg, err := sla.DecodeSignature(body)
	if err != nil {
		return fail(http.StatusBadRequest, "signature: %v", err)
	}
	if sg.StatementSHA256 != sha {
		return fail(http.StatusBadRequest, "the signature is for statement %s", sg.StatementSHA256)
	}
	if sg.Party != p.Role {
		return fail(http.StatusForbidden, "%s is a %s and may not upload a %s signature", p.Name, p.Role, sg.Party)
	}
	if p.Role == RoleFinancier {
		if err := s.feedOpen(p.Name); err != nil {
			return err
		}
	}
	stmt, err := s.store.Statement(sha)
	if err != nil {
		return err
	}
	if err := sg.Check(stmt, st.kr.SignerAt); err != nil {
		return fail(http.StatusBadRequest, "signature: %v", err)
	}
	if err := s.store.AddSignature(sg); err != nil {
		return err
	}
	_, set, err := s.signatureSet(sha, st.kr)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, set.CoSign)
	return nil
}

func (s *Server) postConsent(w http.ResponseWriter, r *http.Request, p *Party, st *state) error {
	body, err := readBody(r, MaxDocumentBytes)
	if err != nil {
		return err
	}
	c, err := DecodeConsent(body)
	if err != nil {
		return fail(http.StatusBadRequest, "consent: %v", err)
	}
	if c.Party != p.Role {
		return fail(http.StatusForbidden, "%s is the %s and may not give consent as the %s", p.Name, p.Role, c.Party)
	}
	if f, ok := st.cfg.ByName(c.Financier); !ok || f.Role != RoleFinancier {
		return fail(http.StatusBadRequest, "%q is not a financier on this ledger", c.Financier)
	}
	if err := c.Check(st.cfg.Contract, st.kr.SignerAt); err != nil {
		return fail(http.StatusBadRequest, "consent: %v", err)
	}
	if err := s.store.AddConsent(c); err != nil {
		return err
	}
	all, err := s.store.Consents()
	if err != nil {
		return err
	}
	open, missing := FeedOpen(all, c.Financier)
	writeJSON(w, http.StatusCreated, map[string]any{"financier": c.Financier, "feedOpen": open, "waitingFor": orEmpty(missing)})
	return nil
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func (s *Server) listConsents(w http.ResponseWriter, _ *http.Request, p *Party, _ *state) error {
	all, err := s.store.Consents()
	if err != nil {
		return err
	}
	out := []*Consent{}
	for _, c := range all {
		if p.Role != RoleFinancier || c.Financier == p.Name {
			out = append(out, c)
		}
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) feedOpen(financier string) error {
	all, err := s.store.Consents()
	if err != nil {
		return err
	}
	if open, missing := FeedOpen(all, financier); !open {
		he := fail(http.StatusForbidden, "the feed for %s is closed: no current consent from %s", financier, strings.Join(missing, " and "))
		he.data = map[string]any{"waitingFor": missing}
		return he
	}
	return nil
}

// Feed is what a financier reads: every sealed statement with its
// signatures, and every checkpoint, with the witness key that signed them.
// It holds no raw logs and nothing the financier could not verify offline.
type Feed struct {
	APIVersion  string             `json:"apiVersion"`
	Kind        string             `json:"kind"`
	Contract    string             `json:"contract"`
	Financier   string             `json:"financier"`
	GeneratedAt string             `json:"generatedAt"`
	Witness     WitnessKey         `json:"witness"`
	Statements  []FeedStatement    `json:"statements"`
	Checkpoints []CheckpointResult `json:"checkpoints"`
}

// WitnessKey is the public key that signs the ledger's checkpoints.
type WitnessKey struct {
	KeyID     string `json:"keyId"`
	PublicKey string `json:"publicKey"`
}

// FeedStatement is one statement in the feed.
type FeedStatement struct {
	Statement  *sla.Statement   `json:"statement"`
	Signatures []*sla.Signature `json:"signatures"`
	CoSigned   bool             `json:"coSigned"`
}

func (s *Server) feed(w http.ResponseWriter, r *http.Request, p *Party, st *state) error {
	financier := p.Name
	if p.Role != RoleFinancier {
		financier = r.URL.Query().Get("financier")
		if f, ok := st.cfg.ByName(financier); !ok || f.Role != RoleFinancier {
			return fail(http.StatusBadRequest, "name the financier whose feed to preview: ?financier=NAME")
		}
	}
	if err := s.feedOpen(financier); err != nil {
		return err
	}
	pemText, err := evidence.PublicKeyPEM(s.witness.Public().(ed25519.PublicKey))
	if err != nil {
		return err
	}
	f := Feed{
		APIVersion: APIVersion, Kind: FeedKind, Contract: st.cfg.Contract, Financier: financier,
		GeneratedAt: s.now().UTC().Format(time.RFC3339), Witness: WitnessKey{KeyID: s.WitnessKeyID(), PublicKey: pemText},
		Statements: []FeedStatement{}, Checkpoints: []CheckpointResult{},
	}
	sts, err := s.store.Statements()
	if err != nil {
		return err
	}
	for _, stmt := range sts {
		_, set, err := s.signatureSet(stmt.StatementSHA256, st.kr)
		if err != nil {
			return err
		}
		f.Statements = append(f.Statements, FeedStatement{Statement: stmt, Signatures: set.Signatures, CoSigned: set.CoSign.OK})
	}
	ids, err := s.store.Checkpoints()
	if err != nil {
		return err
	}
	for _, id := range ids {
		cp, err := s.store.Checkpoint(id)
		if err != nil {
			return err
		}
		f.Checkpoints = append(f.Checkpoints, CheckpointResult{ID: id, Checkpoint: cp})
	}
	writeJSON(w, http.StatusOK, f)
	return nil
}
