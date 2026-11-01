// Package webhook turns webhooks from a ticket or incident system (Jira,
// ServiceNow, PagerDuty, Opsgenie, an in-house tool) into SLA events. A
// mapping file says where in the delivered JSON the node, the ticket status
// and the time are, and which statuses mean that a node went down, came
// back, or entered or left maintenance.
//
// Like the Slurm and Kubernetes watchers it records what the provider's own
// tooling says; it decides nothing about the contract.
package webhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/events"
)

// Mapping document identifiers.
const (
	APIVersion = "capacity-ledger/v1alpha1"
	Kind       = "WebhookMapping"
)

// Mapping says how to read one kind of webhook.
type Mapping struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	// Source is recorded in every event, e.g. "jira-repairs".
	Source string `json:"source"`
	Auth   Auth   `json:"auth"`
	Fields Fields `json:"fields"`
	// Statuses maps a ticket status (as delivered) to an event type:
	// down, up, degraded_start, degraded_end, maintenance_start,
	// maintenance_end. Statuses not listed are acknowledged and ignored.
	Statuses map[string]events.Type `json:"statuses"`
	// CustomerCaused lists statuses whose events are caused by the
	// customer; their cause starts with "customer:", which contracts with
	// exclusions.customerCaused exclude.
	CustomerCaused []string `json:"customerCaused,omitempty"`
	// NodePrefix, if set, is stripped from node values, e.g. "host/".
	NodePrefix string `json:"nodePrefix,omitempty"`

	dir string
}

// Auth says how deliveries prove they come from the ticket system. At least
// one method is required; with both, either is accepted.
type Auth struct {
	// HMACSecretFile holds a shared secret. Deliveries carry
	// hex(HMAC-SHA256(secret, body)) in HMACHeader, optionally prefixed
	// "sha256=" (GitHub, Jira and many others sign this way).
	HMACSecretFile string `json:"hmacSecretFile,omitempty"`
	HMACHeader     string `json:"hmacHeader,omitempty"`
	// TokenSHA256 is the hex SHA-256 of a bearer token sent in the
	// Authorization header (for systems that cannot sign).
	TokenSHA256 string `json:"tokenSha256,omitempty"`

	secret []byte
}

// Fields are dot paths into the delivered JSON, such as
// "issue.fields.customfield_10042" or "event.data.incident.labels.0".
type Fields struct {
	Node     string `json:"node"`
	Status   string `json:"status"`
	Time     string `json:"time,omitempty"`     // default: when the delivery arrived
	Ticket   string `json:"ticket,omitempty"`   // ticket id, recorded in the cause
	Summary  string `json:"summary,omitempty"`  // recorded in the note
	NoticeTS string `json:"noticeTs,omitempty"` // for maintenance_start: when it was announced
}

var (
	sourceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	hexRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// LoadMapping reads and validates a mapping file. A relative secret file is
// resolved against the mapping's directory.
func LoadMapping(path string) (*Mapping, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m Mapping
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m.dir = filepath.Dir(path)
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// Validate checks the mapping and reads the HMAC secret.
func (m *Mapping) Validate() error {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if m.APIVersion != APIVersion || m.Kind != Kind {
		fail("not a %s %s document", APIVersion, Kind)
	}
	if !sourceRe.MatchString(m.Source) {
		fail("source %q must be a lowercase slug such as jira-repairs", m.Source)
	}
	if m.Fields.Node == "" || m.Fields.Status == "" {
		fail("fields.node and fields.status are required")
	}
	if len(m.Statuses) == 0 {
		fail("statuses must map at least one ticket status to an event type")
	}
	for _, st := range m.CustomerCaused {
		if _, ok := m.Statuses[st]; !ok {
			fail("customerCaused: status %q is not in statuses", st)
		}
	}
	for st, typ := range m.Statuses {
		switch typ {
		case events.Down, events.Up, events.DegradedStart, events.DegradedEnd, events.MaintenanceStart, events.MaintenanceEnd:
		default:
			fail("status %q: %q is not an event type a ticket may set", st, typ)
		}
	}
	a := &m.Auth
	if a.HMACSecretFile == "" && a.TokenSHA256 == "" {
		fail("auth needs hmacSecretFile or tokenSha256: the receiver accepts no unauthenticated deliveries")
	}
	if a.TokenSHA256 != "" && !hexRe.MatchString(a.TokenSHA256) {
		fail("auth.tokenSha256 must be 64 lowercase hex characters (capledger ledger token prints one)")
	}
	if a.HMACSecretFile != "" {
		p := a.HMACSecretFile
		if !filepath.IsAbs(p) && m.dir != "" {
			p = filepath.Join(m.dir, p)
		}
		b, err := os.ReadFile(p)
		switch {
		case err != nil:
			fail("auth.hmacSecretFile: %v", err)
		case len(bytes.TrimSpace(b)) < 16:
			fail("auth.hmacSecretFile: the secret must be at least 16 bytes")
		default:
			a.secret = bytes.TrimSpace(b)
		}
		if a.HMACHeader == "" {
			a.HMACHeader = "X-Hub-Signature-256"
		}
	}
	return errors.Join(errs...)
}

// Authenticate checks a delivery's signature or bearer token.
func (a *Auth) Authenticate(body []byte, header func(string) string) bool {
	ok := false
	if len(a.secret) > 0 {
		got := strings.TrimPrefix(strings.TrimSpace(header(a.HMACHeader)), "sha256=")
		mac := hmac.New(sha256.New, a.secret)
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		if subtle.ConstantTimeCompare([]byte(strings.ToLower(got)), []byte(want)) == 1 {
			ok = true
		}
	}
	if a.TokenSHA256 != "" {
		token, found := strings.CutPrefix(header("Authorization"), "Bearer ")
		if found {
			sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
			if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(a.TokenSHA256)) == 1 {
				ok = true
			}
		}
	}
	return ok
}

// Delivery is what a webhook says, once mapped.
type Delivery struct {
	Node     string
	Status   string
	Type     events.Type // empty if the status is not mapped
	TS       int64       // 0 if the delivery carries no time
	NoticeTS int64
	Ticket   string
	Summary  string
	Customer bool // the status is listed in CustomerCaused
}

// Read maps a delivered JSON body.
func (m *Mapping) Read(body []byte) (*Delivery, error) {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("body is not JSON: %v", err)
	}
	get := func(name, path string, required bool) (string, error) {
		if path == "" {
			return "", nil
		}
		v, ok := Lookup(doc, path)
		if !ok || v == "" {
			if required {
				return "", fmt.Errorf("%s: nothing at %q", name, path)
			}
			return "", nil
		}
		return v, nil
	}
	var d Delivery
	var err error
	if d.Node, err = get("node", m.Fields.Node, true); err != nil {
		return nil, err
	}
	d.Node = strings.TrimSpace(strings.TrimPrefix(d.Node, m.NodePrefix))
	if d.Status, err = get("status", m.Fields.Status, true); err != nil {
		return nil, err
	}
	d.Type = m.Statuses[d.Status]
	for _, st := range m.CustomerCaused {
		if st == d.Status {
			d.Customer = true
		}
	}
	d.Ticket, _ = get("ticket", m.Fields.Ticket, false)
	d.Summary, _ = get("summary", m.Fields.Summary, false)
	for _, f := range []struct {
		name, path string
		dst        *int64
	}{{"time", m.Fields.Time, &d.TS}, {"noticeTs", m.Fields.NoticeTS, &d.NoticeTS}} {
		v, _ := get(f.name, f.path, false)
		if v == "" {
			continue
		}
		t, err := ParseTime(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", f.name, err)
		}
		*f.dst = t
	}
	return &d, nil
}

// Lookup follows a dot path ("a.b.0.c") through decoded JSON and returns the
// value as a string. Numbers keep their literal text; objects, arrays and
// null are not values.
func Lookup(doc any, path string) (string, bool) {
	cur := doc
	for _, part := range strings.Split(path, ".") {
		switch v := cur.(type) {
		case map[string]any:
			next, ok := v[part]
			if !ok {
				return "", false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(v) {
				return "", false
			}
			cur = v[i]
		default:
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	}
	return "", false
}

// timeLayouts are the formats ticket systems send: RFC 3339, Jira's
// "2026-10-03T04:12:00.000+0000", and Unix seconds or milliseconds.
var timeLayouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05"}

// ParseTime reads a time in any of the usual ticket-system formats. Times
// without a zone are UTC.
func ParseTime(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n > 1e12 { // milliseconds
			n /= 1000
		}
		return n, nil
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.Unix(), nil
		}
	}
	return 0, fmt.Errorf("unrecognised time %q", s)
}

var causeRe = regexp.MustCompile(`[^A-Za-z0-9_.:/-]+`)

// Cause is the event cause for a delivery: "ticket:<id>" or, without an
// id, "ticket:<status>"; "customer:ticket:..." for customer-caused statuses.
func (d *Delivery) Cause() string {
	v := d.Ticket
	if v == "" {
		v = d.Status
	}
	v = strings.Trim(causeRe.ReplaceAllString(v, "-"), "-")
	if len(v) > 64 {
		v = v[:64]
	}
	if d.Customer {
		return "customer:ticket:" + v
	}
	return "ticket:" + v
}
