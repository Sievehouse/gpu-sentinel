package main

import (
	"crypto/ed25519"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
)

// defaultAgentKeyDir holds the agent's per-node key when --key is not given.
const defaultAgentKeyDir = "/var/lib/capledger/keys"

func cmdKeygen(args []string, s streams) error {
	fs := newFlags("keygen", s)
	out := fs.String("out", "keys", "directory for the key pair")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	priv, pub, id, err := evidence.GenerateKeyPair(*out)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.out, "key id      %s\nprivate key %s (keep secret)\npublic key  %s (share with counterparties)\n", id, priv, pub)
	return nil
}

// signingKey resolves --key or --key-dir. A key directory gets a new key
// pair on first use, so every writer has its own key unless told otherwise.
func signingKey(keyPath, keyDir string, s streams) (ed25519.PrivateKey, error) {
	switch {
	case keyPath != "" && keyDir != "":
		return nil, usagef("give --key or --key-dir, not both")
	case keyPath != "":
		return evidence.LoadPrivateKey(keyPath)
	case keyDir != "":
		priv, created, err := evidence.LoadOrCreateKey(keyDir)
		if err != nil {
			return nil, fmt.Errorf("signing key in %s: %w", keyDir, err)
		}
		if created {
			fmt.Fprintf(s.err, "created signing key %s in %s\n", evidence.KeyID(priv.Public().(ed25519.PublicKey)), keyDir)
		}
		return priv, nil
	}
	return nil, usagef("--key or --key-dir is required")
}

// openLog opens an evidence log for appending and announces the signing key
// in it once, so that a verifier can enrol the key with "keyring import".
func openLog(logPath string, priv ed25519.PrivateKey, node, role string) (*evidence.Writer, error) {
	if logPath == "" {
		return nil, usagef("--log is required")
	}
	w, err := evidence.OpenWriter(logPath, priv)
	if err != nil {
		return nil, err
	}
	if _, err := w.Announce(node, role, "capledger "+version); err != nil {
		return nil, err
	}
	return w, nil
}

// trustFrom builds the verifier's keyring from --keyring and --pub. Keys
// given with --pub are trusted for every node, as in version 0.1.
func trustFrom(keyringPath string, pubs []string) (*keyring.Keyring, error) {
	if keyringPath == "" && len(pubs) == 0 {
		return nil, usagef("--keyring or --pub is required: verification needs to know which keys to trust")
	}
	k := keyring.New()
	if keyringPath != "" {
		loaded, err := keyring.Load(keyringPath)
		if err != nil {
			return nil, err
		}
		k = loaded
	}
	if len(pubs) > 0 {
		extra, err := keyring.FromPublicKeyFiles(pubs, "command line")
		if err != nil {
			return nil, err
		}
		if err := k.Merge(extra); err != nil {
			return nil, err
		}
	}
	return k, nil
}

func cmdKeyringAdd(args []string, s streams) error {
	fs := newFlags("keyring add", s)
	path := fs.String("keyring", "", "keyring file (created if missing)")
	pub := fs.String("pub", "", "public key to trust (PEM)")
	owner := fs.String("owner", "", "who holds the key, e.g. \"Example Neocloud\"")
	role := fs.String("role", "", "provider-agent, provider-integration, tenant-probe, harness or auditor")
	var nodes stringList
	fs.Var(&nodes, "node", "node the key may speak for; repeat (default: any node)")
	notBefore := fs.String("not-before", "", "key valid from (RFC 3339)")
	notAfter := fs.String("not-after", "", "key valid until, exclusive (RFC 3339)")
	note := fs.String("note", "", "free-text note")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("keyring", *path, "pub", *pub, "owner", *owner, "role", *role); err != nil {
		return err
	}
	k, err := keyring.LoadOrNew(*path)
	if err != nil {
		return err
	}
	key, err := evidence.LoadPublicKey(*pub)
	if err != nil {
		return err
	}
	pemText, err := evidence.PublicKeyPEM(key)
	if err != nil {
		return err
	}
	e := keyring.Entry{PublicKey: pemText, Owner: *owner, Role: *role, Nodes: nodes, NotBefore: *notBefore, NotAfter: *notAfter, Note: *note}
	if err := k.Add(e); err != nil {
		return err
	}
	if err := k.Save(*path); err != nil {
		return err
	}
	fmt.Fprintf(s.out, "trusted key %s: %s, %s, nodes %s\n", evidence.KeyID(key), *owner, *role, nodeList(nodes))
	return nil
}

func cmdKeyringImport(args []string, s streams) error {
	fs := newFlags("keyring import", s)
	path := fs.String("keyring", "", "keyring file (created if missing)")
	var logs, only stringList
	fs.Var(&logs, "log", "evidence log with key announcements; repeat for several logs")
	fs.Var(&only, "key-id", "import only this key; repeat (default: every announced key)")
	owner := fs.String("owner", "", "who holds the keys, e.g. \"Example Neocloud\"")
	role := fs.String("role", "", "role for the imported keys, e.g. provider-agent or tenant-probe")
	bind := fs.Bool("bind-nodes", true, "allow each key to speak only for the node(s) it announced")
	notBefore := fs.String("not-before", "", "keys valid from (RFC 3339)")
	notAfter := fs.String("not-after", "", "keys valid until, exclusive (RFC 3339)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("keyring", *path, "owner", *owner, "role", *role); err != nil {
		return err
	}
	if len(logs) == 0 {
		return usagef("--log is required")
	}
	k, err := keyring.LoadOrNew(*path)
	if err != nil {
		return err
	}
	type found struct {
		pem      string
		nodes    map[string]bool
		software string
		where    []string
	}
	keys := map[string]*found{}
	var bad []string
	for _, lp := range logs {
		entries, err := evidence.ReadFile(lp)
		if err != nil {
			return err
		}
		anns, errs := keyring.FindAnnouncements(entries)
		for _, e := range errs {
			bad = append(bad, fmt.Sprintf("%s: %v", lp, e))
		}
		for _, a := range anns {
			f := keys[a.Announcement.KeyID]
			if f == nil {
				f = &found{pem: a.Announcement.PublicKey, nodes: map[string]bool{}, software: a.Announcement.Software}
				keys[a.Announcement.KeyID] = f
			}
			if a.Announcement.Node != "" {
				f.nodes[a.Announcement.Node] = true
			}
			f.where = append(f.where, fmt.Sprintf("%s seq %d", lp, a.Seq))
		}
	}
	if len(bad) > 0 {
		for _, b := range bad {
			fmt.Fprintln(s.err, b)
		}
		return codedError{1, "invalid key announcements found; nothing imported"}
	}
	want := map[string]bool{}
	for _, id := range only {
		if keys[id] == nil {
			return fmt.Errorf("key %s is not announced in the given logs", id)
		}
		want[id] = true
	}
	added := 0
	for _, id := range sortedKeys(keys) {
		f := keys[id]
		if len(want) > 0 && !want[id] {
			continue
		}
		if e, ok := k.Lookup(id); ok {
			fmt.Fprintf(s.out, "already trusted %s (%s, %s)\n", id, e.Owner, e.Role)
			continue
		}
		var nodes []string
		if *bind {
			nodes = sortedKeys(f.nodes)
		}
		switch {
		case *bind && len(f.nodes) > 1:
			fmt.Fprintf(s.err, "warning: key %s announced itself for %d nodes (%s); per-node keys are safer\n", id, len(f.nodes), strings.Join(nodes, ", "))
		case *bind && len(f.nodes) == 0:
			fmt.Fprintf(s.err, "note: key %s announced no node, so it may speak for any node\n", id)
		}
		e := keyring.Entry{
			PublicKey: f.pem, Owner: *owner, Role: *role, Nodes: nodes, NotBefore: *notBefore, NotAfter: *notAfter,
			Note: "announced in " + strings.Join(f.where, "; ") + softwareNote(f.software),
		}
		if err := k.Add(e); err != nil {
			return err
		}
		added++
		fmt.Fprintf(s.out, "trusted key %s: %s, %s, nodes %s\n", id, *owner, *role, nodeList(nodes))
	}
	if added == 0 {
		fmt.Fprintln(s.out, "no new keys")
		return nil
	}
	if err := k.Save(*path); err != nil {
		return err
	}
	fmt.Fprintf(s.out, "%d key(s) added to %s\n", added, *path)
	return nil
}

func softwareNote(sw string) string {
	if sw == "" {
		return ""
	}
	return " by " + sw
}

func nodeList(nodes []string) string {
	if len(nodes) == 0 {
		return "any"
	}
	sorted := append([]string(nil), nodes...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

func cmdKeyringList(args []string, s streams) error {
	fs := newFlags("keyring list", s)
	path := fs.String("keyring", "", "keyring file")
	format := fs.String("format", "text", "text or json")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("keyring", *path); err != nil {
		return err
	}
	if err := checkFormat(*format); err != nil {
		return err
	}
	k, err := keyring.Load(*path)
	if err != nil {
		return err
	}
	if *format == "json" {
		return writeJSON(s.out, k)
	}
	if len(k.Keys) == 0 {
		fmt.Fprintln(s.out, "no keys")
		return nil
	}
	tw := tabwriter.NewWriter(s.out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY ID\tOWNER\tROLE\tNODES\tVALID")
	for _, e := range k.Keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.KeyID, e.Owner, e.Role, nodeList(e.Nodes), validity(e.NotBefore, e.NotAfter))
	}
	return tw.Flush()
}

func validity(from, until string) string {
	switch {
	case from == "" && until == "":
		return "always"
	case until == "":
		return "from " + from
	case from == "":
		return "until " + until
	}
	return from + " to " + until
}
