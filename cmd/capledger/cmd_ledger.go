package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
	"github.com/Sievehouse/gpu-sentinel/internal/ledger"
	"github.com/Sievehouse/gpu-sentinel/internal/sla"
)

func cmdLedgerServe(args []string, s streams) error {
	fs := newFlags("ledger serve", s)
	var configs stringList
	fs.Var(&configs, "config", "ledger configuration (JSON): the contract and its parties; repeat to host several contracts")
	configDir := fs.String("config-dir", "", "host every *.json configuration in this directory; SIGHUP re-reads it, so contracts can be added and removed")
	keyringPath := fs.String("keyring", "", "keyring of every key whose evidence, signatures and consents the ledger accepts (for contracts whose configuration names no keyring of its own)")
	dataDir := fs.String("data", "", "data directory (created if missing)")
	keyDir := fs.String("key-dir", "", "the ledger's own witness key, which signs checkpoints (created on first use)")
	listen := fs.String("listen", "127.0.0.1:8443", "address to listen on")
	tlsCert := fs.String("tls-cert", "", "TLS certificate (PEM); without it, serve behind a TLS-terminating proxy")
	tlsKey := fs.String("tls-key", "", "TLS private key (PEM)")
	cpEvery := fs.Duration("checkpoint-every", 24*time.Hour, "sign a checkpoint of every changed log this often; 0 disables")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("data", *dataDir, "key-dir", *keyDir); err != nil {
		return err
	}
	switch {
	case len(configs) == 0 && *configDir == "":
		return usagef("--config or --config-dir is required")
	case len(configs) > 0 && *configDir != "":
		return usagef("give --config or --config-dir, not both")
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		return usagef("give both --tls-cert and --tls-key, or neither")
	}
	multi := len(configs) > 1 || *configDir != ""
	load := func() ([]ledger.HubContract, error) {
		return loadLedgerContracts(configs, *configDir, *keyringPath)
	}
	contracts, err := load()
	if err != nil {
		return err
	}
	witness, err := signingKey("", *keyDir, s)
	if err != nil {
		return err
	}

	var handler http.Handler
	var reload func() error
	var checkpoint func()
	if !multi {
		c := contracts[0]
		store, err := ledger.OpenStore(*dataDir)
		if err != nil {
			return err
		}
		srv, err := ledger.NewServer(store, c.Config, c.Keyring, witness, s.err)
		if err != nil {
			return err
		}
		handler = srv.Handler()
		reload = func() error {
			cs, err := load()
			if err == nil {
				err = srv.Reload(cs[0].Config, cs[0].Keyring)
			}
			if err == nil {
				fmt.Fprintf(s.err, "reloaded the configuration of contract %s (%d keys)\n", cs[0].Config.Contract, len(cs[0].Keyring.Keys))
			}
			return err
		}
		checkpoint = func() {
			res, err := srv.Checkpoint(false)
			switch {
			case err != nil:
				fmt.Fprintf(s.err, "checkpoint failed: %v\n", err)
			case res != nil:
				fmt.Fprintf(s.err, "checkpoint %s: %d log(s)\n", res.ID, len(res.Checkpoint.Logs))
			}
		}
	} else {
		hub := ledger.NewHub(*dataDir, witness, s.err)
		if _, _, err := hub.Apply(contracts); err != nil {
			return err
		}
		handler = hub.Handler()
		reload = func() error {
			cs, err := load()
			if err != nil {
				return err
			}
			added, removed, err := hub.Apply(cs)
			if err == nil {
				fmt.Fprintf(s.err, "reloaded %d contract(s); added %s; removed %s\n", len(cs), listOrNone(added), listOrNone(removed))
			}
			return err
		}
		checkpoint = func() {
			res, err := hub.Checkpoint(false)
			if err != nil {
				fmt.Fprintf(s.err, "checkpoint failed: %v\n", err)
			}
			for _, id := range sortedKeys(res) {
				fmt.Fprintf(s.err, "contract %s: checkpoint %s: %d log(s)\n", id, res[id].ID, len(res[id].Checkpoint.Logs))
			}
		}
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		for range hup {
			if err := reload(); err != nil {
				fmt.Fprintf(s.err, "reload failed, keeping the old configuration: %v\n", err)
			}
		}
	}()
	if *cpEvery > 0 {
		go every(ctx, *cpEvery, checkpoint)
	}
	errc := make(chan error, 1)
	go func() {
		if *tlsCert != "" {
			errc <- hs.ServeTLS(ln, *tlsCert, *tlsKey)
		} else {
			errc <- hs.Serve(ln)
		}
	}()
	scheme := "https"
	if *tlsCert == "" {
		scheme = "http"
		fmt.Fprintln(s.err, "warning: serving plain HTTP; put the ledger behind a TLS-terminating proxy")
	}
	witnessID := evidence.KeyID(witness.Public().(ed25519.PublicKey))
	if !multi {
		cfg := contracts[0].Config
		fmt.Fprintf(s.err, "capledger ledger %s: contract %s, %d parties, witness key %s, %s://%s\n",
			version, cfg.Contract, len(cfg.Parties), witnessID, scheme, ln.Addr())
	} else {
		fmt.Fprintf(s.err, "capledger ledger %s: %d contracts, witness key %s, %s://%s (each contract at /c/<contract>)\n",
			version, len(contracts), witnessID, scheme, ln.Addr())
		for _, c := range contracts {
			fmt.Fprintf(s.err, "  %s://%s/c/%s  (%d parties)\n", scheme, ln.Addr(), c.Config.Contract, len(c.Config.Parties))
		}
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}

func listOrNone(v []string) string {
	if len(v) == 0 {
		return "none"
	}
	return strings.Join(v, ", ")
}

// loadLedgerContracts reads the configurations and their keyrings. A
// configuration's own keyring wins over --keyring; each keyring file is read
// once.
func loadLedgerContracts(configs []string, configDir, keyringPath string) ([]ledger.HubContract, error) {
	paths := []string(configs)
	if configDir != "" {
		found, err := filepath.Glob(filepath.Join(configDir, "*.json"))
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			return nil, fmt.Errorf("%s holds no *.json configurations", configDir)
		}
		sort.Strings(found)
		paths = found
	}
	keyrings := map[string]*keyring.Keyring{}
	var out []ledger.HubContract
	for _, p := range paths {
		cfg, err := ledger.LoadConfig(p)
		if err != nil {
			return nil, err
		}
		krPath := cfg.KeyringPath()
		if krPath == "" {
			krPath = keyringPath
		}
		if krPath == "" {
			return nil, usagef("%s: contract %s names no keyring; give --keyring or set \"keyring\" in its configuration", p, cfg.Contract)
		}
		kr, ok := keyrings[krPath]
		if !ok {
			if kr, err = keyring.Load(krPath); err != nil {
				return nil, err
			}
			keyrings[krPath] = kr
		}
		out = append(out, ledger.HubContract{Config: cfg, Keyring: kr})
	}
	return out, nil
}

func cmdLedgerToken(args []string, s streams) error {
	fs := newFlags("ledger token", s)
	out := fs.String("out", "", "write the token to this file (mode 0600) instead of printing it")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	if *out != "" {
		if err := os.WriteFile(*out, []byte(token+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(s.out, "token written to %s (give it to the party; keep it secret)\n", *out)
	} else {
		fmt.Fprintf(s.out, "token        %s  (give it to the party; keep it secret)\n", token)
	}
	fmt.Fprintf(s.out, "tokenSha256  %s  (put this in the ledger configuration)\n", ledger.TokenHash(token))
	return nil
}

// clientFlags are the connection flags every ledger client command takes.
type clientFlags struct{ server, tokenFile *string }

func addClientFlags(fs *flag.FlagSet) clientFlags {
	return clientFlags{
		server:    fs.String("server", "", "ledger URL, e.g. https://ledger.example.com"),
		tokenFile: fs.String("token-file", "", "file holding your bearer token (default: $CAPLEDGER_TOKEN)"),
	}
}

func (c clientFlags) client() (*ledger.Client, error) {
	if *c.server == "" {
		return nil, usagef("--server is required")
	}
	token := os.Getenv("CAPLEDGER_TOKEN")
	if *c.tokenFile != "" {
		b, err := os.ReadFile(*c.tokenFile)
		if err != nil {
			return nil, err
		}
		token = string(b)
	}
	if strings.TrimSpace(token) == "" {
		return nil, usagef("--token-file (or $CAPLEDGER_TOKEN) is required")
	}
	cl, err := ledger.NewClient(*c.server, token)
	if err != nil {
		return nil, usageError{err.Error()}
	}
	return cl, nil
}

// logLines returns the non-empty lines of a log file.
func logLines(path string) ([][]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) > 0 {
			out = append(out, append([]byte(nil), line...))
		}
	}
	return out, sc.Err()
}

const pushChunk = 8 << 20

func cmdLedgerPush(args []string, s streams) error {
	fs := newFlags("ledger push", s)
	cf := addClientFlags(fs)
	var logs stringList
	fs.Var(&logs, "log", "evidence log to push; repeat for several (each is named after its file, without .jsonl)")
	name := fs.String("name", "", "name for the log on the ledger (only with a single --log)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if len(logs) == 0 {
		return usagef("--log is required")
	}
	if *name != "" && len(logs) > 1 {
		return usagef("--name works with a single --log")
	}
	cl, err := cf.client()
	if err != nil {
		return err
	}
	for _, path := range logs {
		n := evidence.LogName(path)
		if *name != "" {
			n = *name
		}
		if err := pushOne(cl, path, n, s); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	return nil
}

func pushOne(cl *ledger.Client, path, name string, s streams) error {
	entries, err := evidence.ReadFile(path)
	if err != nil {
		return err
	}
	lines, err := logLines(path)
	if err != nil {
		return err
	}
	head, _, err := cl.Head(name)
	if err != nil {
		return err
	}
	if int64(len(entries)) < head.Entries {
		return fmt.Errorf("the ledger holds %d entries of %s but this file has only %d", head.Entries, name, len(entries))
	}
	if head.Entries > 0 && entries[head.Entries-1].Hash != head.LastHash {
		return fmt.Errorf("this file does not extend the ledger's copy of %s: entry %d differs", name, head.Entries)
	}
	pending := lines[head.Entries:]
	if len(pending) == 0 {
		fmt.Fprintf(s.out, "%s: up to date (%d entries)\n", name, head.Entries)
		return nil
	}
	pushed := 0
	for len(pending) > 0 {
		var buf bytes.Buffer
		k := 0
		for k < len(pending) && (k == 0 || buf.Len()+len(pending[k]) < pushChunk) {
			buf.Write(pending[k])
			buf.WriteByte('\n')
			k++
		}
		info, err := cl.Push(name, buf.Bytes())
		if err != nil {
			return err
		}
		head = info
		pending = pending[k:]
		pushed += k
	}
	fmt.Fprintf(s.out, "%s: pushed %d entries; the ledger holds %d, merkle root %s\n", name, pushed, head.Entries, head.MerkleRoot)
	return nil
}

func cmdLedgerPull(args []string, s streams) error {
	fs := newFlags("ledger pull", s)
	cf := addClientFlags(fs)
	var names stringList
	fs.Var(&names, "name", "log to fetch; repeat for several")
	all := fs.Bool("all", false, "fetch every log on the ledger")
	outDir := fs.String("out-dir", ".", "directory for NAME.jsonl files; existing files are extended, never rewritten")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *all == (len(names) > 0) {
		return usagef("give --name (repeatable) or --all")
	}
	cl, err := cf.client()
	if err != nil {
		return err
	}
	if *all {
		logs, err := cl.Logs()
		if err != nil {
			return err
		}
		for _, l := range logs {
			names = append(names, l.Name)
		}
	}
	if err := os.MkdirAll(*outDir, 0o750); err != nil {
		return err
	}
	for _, n := range names {
		if !evidence.ValidLogName(n) {
			return fmt.Errorf("log name %q", n)
		}
		path := filepath.Join(*outDir, n+".jsonl")
		if err := pullOne(cl, n, path, s); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	fmt.Fprintln(s.err, "verify what you fetched: capledger evidence verify --keyring ... --log ...")
	return nil
}

func pullOne(cl *ledger.Client, name, path string, s streams) error {
	var local []evidence.Entry
	if _, err := os.Stat(path); err == nil {
		if local, err = evidence.ReadFile(path); err != nil {
			return err
		}
	}
	raw, err := cl.Pull(name, int64(len(local)))
	if err != nil {
		return err
	}
	fresh, err := evidence.ReadEntries(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	if len(fresh) == 0 {
		head, _, err := cl.Head(name)
		if err != nil {
			return err
		}
		if head.Entries < int64(len(local)) {
			return fmt.Errorf("the ledger holds %d entries but %s has %d; refusing to touch it", head.Entries, path, len(local))
		}
		if head.Entries > 0 && local[head.Entries-1].Hash != head.LastHash {
			return fmt.Errorf("%s differs from the ledger's copy", path)
		}
		fmt.Fprintf(s.out, "%s: up to date (%d entries)\n", name, len(local))
		return nil
	}
	prev := evidence.GenesisPrev
	if len(local) > 0 {
		prev = local[len(local)-1].Hash
	}
	if fresh[0].Prev != prev {
		return fmt.Errorf("the ledger's entries do not continue %s; it differs from the ledger's copy", path)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(s.out, "%s: fetched %d entries into %s (%d in all)\n", name, len(fresh), path, len(local)+len(fresh))
	return nil
}

func cmdLedgerCheckpoint(args []string, s streams) error {
	fs := newFlags("ledger checkpoint", s)
	cf := addClientFlags(fs)
	create := fs.Bool("create", false, "ask the ledger's witness to sign a new checkpoint now (default: fetch the latest)")
	out := fs.String("out", "", "write the checkpoint here, for evidence verify --checkpoint")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("out", *out); err != nil {
		return err
	}
	cl, err := cf.client()
	if err != nil {
		return err
	}
	var res *ledger.CheckpointResult
	if *create {
		res, err = cl.CreateCheckpoint()
	} else {
		res, err = cl.LatestCheckpoint()
	}
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(res.Checkpoint, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(s.out, "checkpoint %s: %d log(s), signed by witness key %s: %s\n", res.ID, len(res.Checkpoint.Logs), res.Checkpoint.KeyID, *out)
	return nil
}

func cmdLedgerPublish(args []string, s streams) error {
	fs := newFlags("ledger publish", s)
	cf := addClientFlags(fs)
	stmtPath := fs.String("statement", "", "sealed statement to upload")
	var sigs stringList
	fs.Var(&sigs, "signature", "signature to upload (from sla sign); repeat as needed")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *stmtPath == "" && len(sigs) == 0 {
		return usagef("give --statement, --signature or both")
	}
	cl, err := cf.client()
	if err != nil {
		return err
	}
	if *stmtPath != "" {
		st, err := sla.ReadStatement(*stmtPath)
		if err != nil {
			return err
		}
		created, err := cl.PutStatement(st)
		if err != nil {
			return err
		}
		verb := "already on the ledger"
		if created {
			verb = "uploaded"
		}
		fmt.Fprintf(s.out, "statement %s %s\n", st.StatementSHA256, verb)
	}
	for _, p := range sigs {
		sg, err := sla.ReadSignature(p)
		if err != nil {
			return err
		}
		rep, err := cl.PostSignature(sg)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		state := "not yet co-signed (accepted by " + orNone(rep.AcceptedBy) + "; missing " + orNone(rep.Missing) + ")"
		if rep.OK {
			state = "co-signed by " + strings.Join(rep.AcceptedBy, ", ")
		}
		fmt.Fprintf(s.out, "%s signature for %s uploaded: %s\n", sg.Party, short(sg.StatementSHA256), state)
	}
	return nil
}

func cmdLedgerConsent(args []string, s streams) error {
	fs := newFlags("ledger consent", s)
	cf := addClientFlags(fs)
	financier := fs.String("financier", "", "the financier's party name on the ledger")
	party := fs.String("party", "", "provider or customer: the party you consent for (your key's keyring role)")
	revoke := fs.Bool("revoke", false, "withdraw consent instead of granting it")
	note := fs.String("note", "", "free-text note, e.g. the facility agreement it serves")
	keyPath := fs.String("key", "", "the party's private signing key (PEM)")
	keyDir := fs.String("key-dir", "", "directory holding the party's signing key")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("financier", *financier, "party", *party); err != nil {
		return err
	}
	cl, err := cf.client()
	if err != nil {
		return err
	}
	info, err := cl.Info()
	if err != nil {
		return err
	}
	priv, err := signingKey(*keyPath, *keyDir, s)
	if err != nil {
		return err
	}
	decision := ledger.Grant
	if *revoke {
		decision = ledger.Revoke
	}
	c, err := ledger.SignConsent(info.Contract, *financier, *party, decision, *note, priv, time.Now())
	if err != nil {
		return err
	}
	res, err := cl.PostConsent(c)
	if err != nil {
		return err
	}
	state := "closed, waiting for " + strings.Join(res.WaitingFor, " and ")
	if res.FeedOpen {
		state = "open"
	}
	fmt.Fprintf(s.out, "%s %s consent for %s; the feed for %s is %s\n", *party, decision, *financier, *financier, state)
	return nil
}

func cmdLedgerFeed(args []string, s streams) error {
	fs := newFlags("ledger feed", s)
	cf := addClientFlags(fs)
	financier := fs.String("financier", "", "preview this financier's feed (providers, customers and auditors only)")
	outDir := fs.String("out-dir", "", "write the feed as files, ready for sla verify and evidence verify")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("out-dir", *outDir); err != nil {
		return err
	}
	cl, err := cf.client()
	if err != nil {
		return err
	}
	f, err := cl.Feed(*financier)
	if err != nil {
		return err
	}
	write := func(rel string, v any) error {
		p := filepath.Join(*outDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return err
		}
		var b []byte
		if str, ok := v.(string); ok {
			b = []byte(str)
		} else if b, err = json.MarshalIndent(v, "", "  "); err != nil {
			return err
		} else {
			b = append(b, '\n')
		}
		return os.WriteFile(p, b, 0o644)
	}
	if err := write("feed.json", f); err != nil {
		return err
	}
	if err := write("witness.pub.pem", f.Witness.PublicKey); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(s.out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "PERIOD\tAVAILABILITY\tCREDIT\tCO-SIGNED\tSTATEMENT\n")
	for _, item := range f.Statements {
		st := item.Statement
		base := filepath.Join("statements", st.Period.Start[:7]+"-"+short(st.StatementSHA256))
		if err := write(base+".json", st); err != nil {
			return err
		}
		for i, sg := range item.Signatures {
			if err := write(fmt.Sprintf("%s.%d.%s.sig.json", base, i+1, sg.Party), sg); err != nil {
				return err
			}
		}
		fmt.Fprintf(tw, "%s\t%s%%\t%d%% %s\t%v\t%s\n", st.Period.Start[:7], st.Summary.AvailabilityPercent, st.Credit.Percent, st.Credit.Amount, item.CoSigned, base+".json")
	}
	tw.Flush()
	for _, cp := range f.Checkpoints {
		if err := write(filepath.Join("checkpoints", cp.ID+".json"), cp.Checkpoint); err != nil {
			return err
		}
	}
	fmt.Fprintf(s.out, "%d statement(s), %d checkpoint(s) for %s in %s; witness key %s\n", len(f.Statements), len(f.Checkpoints), f.Financier, *outDir, f.Witness.KeyID)
	fmt.Fprintln(s.out, "check them yourself: capledger sla verify --statement S --signature SIG ... --keyring K --require provider,customer")
	return nil
}

func cmdLedgerStatus(args []string, s streams) error {
	fs := newFlags("ledger status", s)
	cf := addClientFlags(fs)
	witnessOut := fs.String("witness-pub", "", "also save the ledger's witness public key (PEM) here, to enrol it with keyring add --role witness")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cl, err := cf.client()
	if err != nil {
		return err
	}
	info, err := cl.Info()
	if err != nil {
		return err
	}
	fmt.Fprintf(s.out, "ledger    contract %s; you are %s (%s); witness key %s\n", info.Contract, info.Party.Name, info.Party.Role, info.Witness.KeyID)
	if *witnessOut != "" {
		if err := os.WriteFile(*witnessOut, []byte(info.Witness.PublicKey), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(s.out, "witness public key saved to %s; compare its key id with the operator's before enrolling it\n", *witnessOut)
	}
	if info.Party.Role == ledger.RoleFinancier {
		fmt.Fprintln(s.out, "financiers read the ledger through: capledger ledger feed")
		return nil
	}
	logs, err := cl.Logs()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(s.out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "\nLOG\tOWNER\tENTRIES\tMERKLE ROOT\n")
	for _, l := range logs {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", l.Name, l.Owner, l.Entries, l.MerkleRoot)
	}
	tw.Flush()
	if cp, err := cl.LatestCheckpoint(); err == nil {
		fmt.Fprintf(s.out, "\nlatest checkpoint %s (%d logs)\n", cp.ID, len(cp.Checkpoint.Logs))
	} else if !ledger.NotFound(err) {
		return err
	}
	sts, err := cl.Statements()
	if err != nil {
		return err
	}
	tw = tabwriter.NewWriter(s.out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "\nSTATEMENT\tPERIOD\tAVAILABILITY\tCREDIT\tACCEPTED BY\tDISPUTED BY\n")
	for _, st := range sts {
		set, err := cl.Signatures(st.StatementSHA256)
		if err != nil {
			return err
		}
		fmt.Fprintf(tw, "%s\t%s\t%s%%\t%d%%\t%s\t%s\n", short(st.StatementSHA256), st.PeriodStart[:7], st.AvailabilityPercent, st.CreditPercent,
			orNone(set.CoSign.AcceptedBy), orNone(set.CoSign.DisputedBy))
	}
	return tw.Flush()
}
