package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/contract"
	"github.com/Sievehouse/gpu-sentinel/internal/events"
	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
	"github.com/Sievehouse/gpu-sentinel/internal/webhook"
)

func cmdWatchWebhook(args []string, s streams) error {
	fs := newFlags("watch webhook", s)
	mappingPath := fs.String("mapping", "", "webhook mapping (JSON): where the node, status and time are, and which statuses mean down or up")
	listen := fs.String("listen", "127.0.0.1:8090", "address to receive deliveries on")
	tlsCert := fs.String("tls-cert", "", "TLS certificate (PEM); without it, receive behind a TLS-terminating proxy")
	tlsKey := fs.String("tls-key", "", "TLS private key (PEM)")
	var replay stringList
	fs.Var(&replay, "replay", "instead of listening, process this saved delivery (JSON) and exit; repeatable, in order; not authenticated")
	contractPath := fs.String("contract", "", "accept only the contract's nodes")
	logPath := fs.String("log", "", "evidence log to append to (the receiver's own)")
	keyPath := fs.String("key", "", "private signing key (PEM)")
	keyDir := fs.String("key-dir", "", "directory for the receiver's own signing key, created on first use (default "+defaultWatcherKeyDir+" unless --key is given)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := required("mapping", *mappingPath); err != nil {
		return err
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		return usagef("give both --tls-cert and --tls-key, or neither")
	}
	m, err := webhook.LoadMapping(*mappingPath)
	if err != nil {
		return err
	}
	var scope []string
	if *contractPath != "" {
		spec, err := contract.Load(*contractPath)
		if err != nil {
			return err
		}
		scope = spec.SortedNodes()
	}
	if *keyPath == "" && *keyDir == "" {
		*keyDir = defaultWatcherKeyDir
	}
	priv, err := signingKey(*keyPath, *keyDir, s)
	if err != nil {
		return err
	}
	w, err := openLog(*logPath, priv, "", keyring.RoleProviderIntegration)
	if err != nil {
		return err
	}
	record := func(ev events.Event) (string, error) {
		ev.ID, ev.Seq = fmt.Sprintf("%s-%d", w.KeyID(), w.NextSeq()), w.NextSeq()
		if err := ev.Normalize(); err != nil {
			return "", err
		}
		if _, err := w.Append(evidence.KindEvent, ev); err != nil {
			return "", err
		}
		return ev.ID, nil
	}
	rcv := webhook.NewReceiver(m, scope, record, s.err)
	own, err := ownEvents(*logPath, w.KeyID())
	if err != nil {
		return err
	}
	if err := rcv.Resume(own); err != nil {
		return err
	}

	if len(replay) > 0 {
		for _, p := range replay {
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			d, err := m.Read(body)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			code, res := rcv.Apply(d)
			switch {
			case res.Recorded:
				fmt.Fprintf(s.out, "%s: %s %s %s (%s)\n", p, res.TS, res.Node, res.Type, res.Event)
			case res.Error != "":
				return fmt.Errorf("%s: %s (HTTP %d)", p, res.Error, code)
			default:
				fmt.Fprintf(s.out, "%s: not recorded: %s\n", p, res.Reason)
			}
		}
		return nil
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: rcv, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
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
	}
	fmt.Fprintf(s.err, "capledger watch webhook %s: source %s, key %s, %d node(s) with an open stretch, %s://%s\n",
		version, m.Source, w.KeyID(), len(rcv.Open()), scheme, ln.Addr())
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(shutdown)
}

// ownEvents returns the events the key keyID wrote to the log at path.
func ownEvents(path, keyID string) ([]events.Event, error) {
	entries, err := evidence.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []events.Event
	for _, e := range entries {
		if e.KeyID != keyID || e.Kind != evidence.KindEvent {
			continue
		}
		var ev events.Event
		if err := e.DecodePayload(&ev); err != nil {
			return nil, fmt.Errorf("%s entry %d: %w", path, e.Seq, err)
		}
		out = append(out, ev)
	}
	return out, nil
}
