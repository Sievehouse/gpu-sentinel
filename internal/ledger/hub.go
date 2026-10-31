package ledger

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/keyring"
)

// ContractIDPattern is what a contract id must look like to be hosted by a
// Hub, where it becomes a URL path segment and a directory name.
var contractIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidContractID reports whether id can be hosted by a Hub.
func ValidContractID(id string) bool {
	return contractIDRe.MatchString(id) && !strings.Contains(id, "..")
}

// HubContract is one contract a Hub serves: its configuration and the
// keyring its server verifies against.
type HubContract struct {
	Config  *Config
	Keyring *keyring.Keyring
}

// Hub serves several contracts from one process. Each contract keeps its own
// configuration, keyring, store (DATA/contracts/ID) and API, at
// /c/ID/v1/...; the parties of one contract cannot see another. One witness
// key signs the checkpoints of all of them, naming the contract in each.
type Hub struct {
	dataDir string
	witness ed25519.PrivateKey
	logw    io.Writer

	mu      sync.RWMutex
	servers map[string]*Server
	now     func() time.Time
}

// NewHub returns an empty hub storing its contracts under dataDir.
func NewHub(dataDir string, witness ed25519.PrivateKey, logw io.Writer) *Hub {
	if logw == nil {
		logw = io.Discard
	}
	return &Hub{dataDir: dataDir, witness: witness, logw: logw, servers: map[string]*Server{}, now: time.Now}
}

// Apply makes the hub serve exactly the given contracts: it reloads the
// configuration of contracts it already serves, opens new ones and stops
// serving those that are gone (their data stays on disk). Nothing changes
// if any contract is invalid.
func (h *Hub) Apply(contracts []HubContract) (added, removed []string, err error) {
	want := map[string]HubContract{}
	var errs []error
	for _, c := range contracts {
		if c.Config == nil || c.Keyring == nil {
			return nil, nil, errors.New("hub: a contract needs a configuration and a keyring")
		}
		id := c.Config.Contract
		if err := c.Config.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("contract %s: %w", id, err))
			continue
		}
		if !ValidContractID(id) {
			errs = append(errs, fmt.Errorf("contract id %q cannot be hosted with others: use letters, digits, '.', '_' and '-'", id))
			continue
		}
		if _, dup := want[id]; dup {
			errs = append(errs, fmt.Errorf("contract %s is configured twice", id))
			continue
		}
		want[id] = c
	}
	if err := errors.Join(errs...); err != nil {
		return nil, nil, err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	opened := map[string]*Server{}
	for id, c := range want {
		if _, ok := h.servers[id]; ok {
			continue
		}
		store, err := OpenStore(filepath.Join(h.dataDir, "contracts", id))
		if err != nil {
			return nil, nil, fmt.Errorf("contract %s: %w", id, err)
		}
		srv, err := NewServer(store, c.Config, c.Keyring, h.witness, h.logw)
		if err != nil {
			return nil, nil, fmt.Errorf("contract %s: %w", id, err)
		}
		srv.witnessName = "ledger witness for contract " + id
		srv.prefix = "/c/" + id
		srv.now = h.now
		opened[id] = srv
	}
	for id, srv := range h.servers {
		c, ok := want[id]
		if !ok {
			removed = append(removed, id)
			continue
		}
		if err := srv.Reload(c.Config, c.Keyring); err != nil {
			return nil, nil, fmt.Errorf("contract %s: %w", id, err)
		}
	}
	for _, id := range removed {
		delete(h.servers, id)
	}
	for id, srv := range opened {
		h.servers[id] = srv
		added = append(added, id)
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed, nil
}

// Contracts returns the ids of the contracts the hub serves.
func (h *Hub) Contracts() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.servers))
	for id := range h.servers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Server returns the server of one contract.
func (h *Hub) Server(id string) (*Server, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.servers[id]
	return s, ok
}

// SetClock replaces the clock of every contract (for tests).
func (h *Hub) SetClock(now func() time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = now
	for _, s := range h.servers {
		s.now = now
	}
}

// Checkpoint checkpoints every contract (see Server.Checkpoint). Results
// are keyed by contract id; contracts with nothing new are left out.
func (h *Hub) Checkpoint(force bool) (map[string]*CheckpointResult, error) {
	out := map[string]*CheckpointResult{}
	var errs []error
	for _, id := range h.Contracts() {
		srv, ok := h.Server(id)
		if !ok {
			continue
		}
		res, err := srv.Checkpoint(force)
		if err != nil {
			errs = append(errs, fmt.Errorf("contract %s: %w", id, err))
			continue
		}
		if res != nil {
			out[id] = res
		}
	}
	return out, errors.Join(errs...)
}

// Handler returns the hub's HTTP API: GET /healthz, and each contract's API
// under /c/ID. An unknown contract gets the same answer as an unknown
// token, so the hub does not reveal which contracts it serves.
func (h *Hub) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
			io.WriteString(w, "ok\n")
			return
		}
		rest, ok := strings.CutPrefix(r.URL.Path, "/c/")
		id, sub, _ := strings.Cut(rest, "/")
		var srv *Server
		if ok {
			srv, ok = h.Server(id)
		}
		if !ok {
			writeError(w, fail(http.StatusUnauthorized, "missing or unknown bearer token"))
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + sub
		r2.URL.RawPath = ""
		srv.handler().ServeHTTP(w, r2)
	})
}
