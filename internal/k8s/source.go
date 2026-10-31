package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Source fetches the current list of nodes.
type Source interface {
	Nodes(ctx context.Context) (*NodeList, error)
	String() string
}

// FileSource reads a saved "kubectl get nodes -o json" file on every call.
type FileSource struct{ Path string }

// Nodes reads the file.
func (f FileSource) Nodes(context.Context) (*NodeList, error) {
	b, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, err
	}
	return ParseNodeList(b)
}

func (f FileSource) String() string { return "file " + f.Path }

// KubectlSource runs kubectl, with the caller's kubeconfig.
type KubectlSource struct {
	Bin      string // default "kubectl"
	Context  string // --context, optional
	Selector string // -l, optional
}

// Nodes runs "kubectl get nodes -o json".
func (k KubectlSource) Nodes(ctx context.Context) (*NodeList, error) {
	bin := k.Bin
	if bin == "" {
		bin = "kubectl"
	}
	args := []string{"get", "nodes", "-o", "json"}
	if k.Context != "" {
		args = append(args, "--context", k.Context)
	}
	if k.Selector != "" {
		args = append(args, "-l", k.Selector)
	}
	cctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(cctx, bin, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %v: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return ParseNodeList(out)
}

func (k KubectlSource) String() string { return "kubectl" }

// In-cluster service account files.
const (
	ServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// APISource reads nodes from the Kubernetes API server, by default with the
// pod's service account. The token is read on every call, because bound
// service account tokens rotate.
type APISource struct {
	Server    string // e.g. https://10.0.0.1:443
	TokenFile string
	Selector  string
	Client    *http.Client
}

// InCluster returns an APISource for the pod's own cluster.
func InCluster(selector string) (*APISource, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a Kubernetes pod (KUBERNETES_SERVICE_HOST is not set); use --from kubectl or a file")
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return NewAPISource("https://"+host+":"+port, ServiceAccountDir+"/token", ServiceAccountDir+"/ca.crt", selector)
}

// NewAPISource returns a source for server, trusting the CA in caFile (empty
// for the system roots) and authenticating with the token in tokenFile.
func NewAPISource(server, tokenFile, caFile, selector string) (*APISource, error) {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("API server %q must be an https:// URL", server)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no certificates", caFile)
		}
		tlsCfg.RootCAs = pool
	}
	return &APISource{
		Server: strings.TrimRight(server, "/"), TokenFile: tokenFile, Selector: selector,
		Client: &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment}},
	}, nil
}

// Nodes lists the nodes.
func (a *APISource) Nodes(ctx context.Context) (*NodeList, error) {
	u := a.Server + "/api/v1/nodes"
	if a.Selector != "" {
		u += "?labelSelector=" + url.QueryEscape(a.Selector)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if a.TokenFile != "" {
		tok, err := os.ReadFile(a.TokenFile)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("API server: HTTP %d: %s", resp.StatusCode, msg)
	}
	return ParseNodeList(body)
}

func (a *APISource) String() string { return "API server " + a.Server }
