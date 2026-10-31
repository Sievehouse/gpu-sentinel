package ledger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Sievehouse/gpu-sentinel/internal/evidence"
	"github.com/Sievehouse/gpu-sentinel/internal/sla"
)

// Client talks to a ledger server as one party.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewClient returns a client for the server at baseURL.
func NewClient(baseURL, token string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("server %q must be an http:// or https:// URL", baseURL)
	}
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("a bearer token is required")
	}
	return &Client{BaseURL: u.String(), Token: strings.TrimSpace(token), HTTP: &http.Client{Timeout: 5 * time.Minute}}, nil
}

// APIError is an error response from the server.
type APIError struct {
	Status  int
	Message string
	Head    *LogInfo
}

func (e *APIError) Error() string { return fmt.Sprintf("server: %s (HTTP %d)", e.Message, e.Status) }

func (c *Client) do(method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string   `json:"error"`
			Head  *LogInfo `json:"head"`
		}
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(raw))
		}
		return &APIError{Status: resp.StatusCode, Message: e.Error, Head: e.Head}
	}
	switch v := out.(type) {
	case nil:
		return nil
	case *[]byte:
		*v = raw
		return nil
	default:
		return json.Unmarshal(raw, out)
	}
}

// NotFound reports whether err is a 404 from the server.
func NotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// Info describes the ledger and the calling party.
func (c *Client) Info() (*Info, error) {
	var in Info
	return &in, c.do("GET", "/v1/info", nil, &in)
}

// Logs lists the logs on the ledger.
func (c *Client) Logs() ([]LogInfo, error) {
	var out []LogInfo
	return out, c.do("GET", "/v1/logs", nil, &out)
}

// Head returns a log's head; a log the server does not hold yet has an
// empty head and ok false.
func (c *Client) Head(name string) (LogInfo, bool, error) {
	var info LogInfo
	err := c.do("GET", "/v1/logs/"+url.PathEscape(name)+"/head", nil, &info)
	if NotFound(err) {
		return LogInfo{LogHead: evidence.LogHead{Name: name}}, false, nil
	}
	return info, err == nil, err
}

// Push appends entries (JSON Lines) to a log.
func (c *Client) Push(name string, lines []byte) (LogInfo, error) {
	var info LogInfo
	return info, c.do("POST", "/v1/logs/"+url.PathEscape(name), lines, &info)
}

// Pull returns a log's entries from index from (0-based) onwards, as JSON
// Lines.
func (c *Client) Pull(name string, from int64) ([]byte, error) {
	var raw []byte
	return raw, c.do("GET", "/v1/logs/"+url.PathEscape(name)+"?from="+strconv.FormatInt(from, 10), nil, &raw)
}

// CreateCheckpoint asks the ledger's witness to sign every log's head now.
func (c *Client) CreateCheckpoint() (*CheckpointResult, error) {
	var res CheckpointResult
	return &res, c.do("POST", "/v1/checkpoints", []byte("{}"), &res)
}

// LatestCheckpoint returns the newest checkpoint.
func (c *Client) LatestCheckpoint() (*CheckpointResult, error) {
	var res CheckpointResult
	return &res, c.do("GET", "/v1/checkpoints/latest", nil, &res)
}

// PutStatement uploads a sealed statement.
func (c *Client) PutStatement(st *sla.Statement) (created bool, err error) {
	b, err := json.Marshal(st)
	if err != nil {
		return false, err
	}
	var res struct {
		Created bool `json:"created"`
	}
	return res.Created, c.do("PUT", "/v1/statements", b, &res)
}

// Statements lists the statements on the ledger.
func (c *Client) Statements() ([]StatementSummary, error) {
	var out []StatementSummary
	return out, c.do("GET", "/v1/statements", nil, &out)
}

// PostSignature uploads a statement signature and returns the statement's
// co-signing status.
func (c *Client) PostSignature(sg *sla.Signature) (*sla.CoSignReport, error) {
	b, err := json.Marshal(sg)
	if err != nil {
		return nil, err
	}
	var rep sla.CoSignReport
	return &rep, c.do("POST", "/v1/statements/"+sg.StatementSHA256+"/signatures", b, &rep)
}

// Signatures returns a statement's signatures and co-signing status.
func (c *Client) Signatures(sha string) (*SignatureSet, error) {
	var set SignatureSet
	return &set, c.do("GET", "/v1/statements/"+url.PathEscape(sha)+"/signatures", nil, &set)
}

// ConsentResult says whether a financier's feed is open after a consent.
type ConsentResult struct {
	Financier  string   `json:"financier"`
	FeedOpen   bool     `json:"feedOpen"`
	WaitingFor []string `json:"waitingFor"`
}

// PostConsent uploads a signed feed consent.
func (c *Client) PostConsent(con *Consent) (*ConsentResult, error) {
	b, err := json.Marshal(con)
	if err != nil {
		return nil, err
	}
	var res ConsentResult
	return &res, c.do("POST", "/v1/consents", b, &res)
}

// Feed fetches the financier feed. Parties other than the financier name
// the financier whose feed they want to preview.
func (c *Client) Feed(financier string) (*Feed, error) {
	path := "/v1/feed"
	if financier != "" {
		path += "?financier=" + url.QueryEscape(financier)
	}
	var f Feed
	return &f, c.do("GET", path, nil, &f)
}
