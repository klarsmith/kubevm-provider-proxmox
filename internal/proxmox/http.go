// SPDX-License-Identifier: Apache-2.0

package proxmox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Credentials are read from the Secret named by spec.credentialsSecretRef.
type Credentials struct {
	URL                string // https://pve.example:8006
	TokenID            string // user@realm!tokenid
	TokenSecret        string
	CABundle           []byte
	InsecureSkipVerify bool
}

// HTTPClient talks to the Proxmox VE REST API with an API token.
type HTTPClient struct {
	base  string
	auth  string
	httpc *http.Client
}

var _ Client = (*HTTPClient)(nil)

// NewHTTPClient builds a client for one Proxmox endpoint.
func NewHTTPClient(c Credentials) (*HTTPClient, error) {
	if c.URL == "" || c.TokenID == "" || c.TokenSecret == "" {
		return nil, errors.New("credentials need url, tokenID and tokenSecret")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(c.CABundle) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(c.CABundle) {
			return nil, errors.New("caBundle holds no PEM certificates")
		}
		tlsCfg.RootCAs = pool
	}
	// Opt-in only: a fresh PVE install has a self-signed certificate.
	tlsCfg.InsecureSkipVerify = c.InsecureSkipVerify //nolint:gosec

	return &HTTPClient{
		base: strings.TrimRight(c.URL, "/") + "/api2/json",
		auth: fmt.Sprintf("PVEAPIToken=%s=%s", c.TokenID, c.TokenSecret),
		httpc: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg, Proxy: http.ProxyFromEnvironment},
		},
	}, nil
}

// do sends one request and decodes the "data" member of the response into
// out (which may be nil).
func (c *HTTPClient) do(ctx context.Context, method, path string,
	params url.Values, out any) error {

	u := c.base + path
	var body io.Reader
	if method == http.MethodGet || method == http.MethodDelete {
		if len(params) > 0 {
			u += "?" + params.Encode()
		}
	} else if params != nil {
		body = strings.NewReader(params.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return fmt.Errorf("building %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", c.auth)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := c.httpc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("reading %s %s: %w", method, path, err)
	}

	if resp.StatusCode != http.StatusOK {
		// Proxmox puts the human-readable reason in the status line and,
		// for parameter errors, an "errors" map in the body.
		msg := fmt.Sprintf("%s %s: %s: %s", method, path, resp.Status,
			strings.TrimSpace(string(raw)))
		// Only a missing VM config or a missing task is "not found". Other
		// "does not exist" errors (a storage, a bridge) are real failures.
		if resp.StatusCode == http.StatusNotFound ||
			(strings.Contains(msg, "does not exist") && strings.Contains(msg, ".conf")) ||
			strings.Contains(msg, "no such task") {
			return fmt.Errorf("%w: %s", ErrNotFound, msg)
		}
		// 400 is a parameter the schema rejected. Some validations run after
		// schema checks and answer 500 with a "validation error" message.
		if resp.StatusCode == http.StatusBadRequest ||
			strings.Contains(msg, "validation error") || strings.Contains(msg, "invalid format") {
			return fmt.Errorf("%w: %s", ErrInvalidParameter, msg)
		}
		return errors.New(msg)
	}

	if out == nil {
		return nil
	}
	envelope := struct {
		Data json.RawMessage `json:"data"`
	}{}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decoding %s %s: %w", method, path, err)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decoding data of %s %s: %w", method, path, err)
	}
	return nil
}

func qemuPath(node string, vmid int, rest string) string {
	return fmt.Sprintf("/nodes/%s/qemu/%d%s", url.PathEscape(node), vmid, rest)
}

// Version returns the PVE release, e.g. "8.2.4".
func (c *HTTPClient) Version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, http.MethodGet, "/version", nil, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// ClusterName returns the cluster name, or the node name of a standalone
// (unclustered) host.
func (c *HTTPClient) ClusterName(ctx context.Context) (string, error) {
	var items []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := c.do(ctx, http.MethodGet, "/cluster/status", nil, &items); err != nil {
		return "", err
	}
	node := ""
	for _, it := range items {
		if it.Type == "cluster" {
			return it.Name, nil
		}
		if it.Type == "node" && node == "" {
			node = it.Name
		}
	}
	if node == "" {
		return "", errors.New("/cluster/status lists neither a cluster nor a node")
	}
	return node, nil
}

// Resources lists every QEMU guest in the cluster, templates included.
func (c *HTTPClient) Resources(ctx context.Context) ([]VM, error) {
	var items []struct {
		Type     string `json:"type"`
		VMID     int    `json:"vmid"`
		Node     string `json:"node"`
		Name     string `json:"name"`
		Pool     string `json:"pool"`
		Status   string `json:"status"`
		Template int    `json:"template"`
	}
	if err := c.do(ctx, http.MethodGet, "/cluster/resources",
		url.Values{"type": {"vm"}}, &items); err != nil {
		return nil, err
	}
	vms := make([]VM, 0, len(items))
	for _, it := range items {
		if it.Type != "qemu" && it.Type != "lxc" {
			continue
		}
		vms = append(vms, VM{
			Container: it.Type == "lxc",
			VMID:      it.VMID, Node: it.Node, Name: it.Name, Pool: it.Pool,
			Status: it.Status, Template: it.Template == 1,
		})
	}
	return vms, nil
}

// CurrentStatus reads one VM's live state from its node, not from the
// cluster-wide resource cache.
func (c *HTTPClient) CurrentStatus(ctx context.Context, node string, vmid int) (string, error) {
	var s struct {
		Status    string `json:"status"`
		QMPStatus string `json:"qmpstatus"`
	}
	if err := c.do(ctx, http.MethodGet, qemuPath(node, vmid, "/status/current"), nil, &s); err != nil {
		return "", err
	}
	// "status" stays "running" for a paused VM; only qmpstatus says so.
	if s.Status == "running" && s.QMPStatus != "" && s.QMPStatus != "running" {
		return "paused", nil
	}
	return s.Status, nil
}

// NextID returns a free VMID. Racy by nature: another client can take the
// same id before the clone lands.
func (c *HTTPClient) NextID(ctx context.Context) (int, error) {
	// Returned as a JSON string on every PVE version seen so far.
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/cluster/nextid", nil, &raw); err != nil {
		return 0, err
	}
	s := strings.Trim(string(raw), `"`)
	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parsing nextid %q: %w", s, err)
	}
	return id, nil
}

// Config returns the VM's current configuration, every value as a string.
func (c *HTTPClient) Config(ctx context.Context, node string, vmid int) (map[string]string, error) {
	var raw map[string]any
	if err := c.do(ctx, http.MethodGet, qemuPath(node, vmid, "/config"), nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		switch t := v.(type) {
		case string:
			out[k] = t
		case float64:
			out[k] = strconv.FormatFloat(t, 'f', -1, 64)
		default:
			out[k] = fmt.Sprint(t)
		}
	}
	return out, nil
}

// SetConfig applies params synchronously (PUT, not the async POST).
func (c *HTTPClient) SetConfig(ctx context.Context, node string, vmid int, params url.Values) error {
	return c.do(ctx, http.MethodPut, qemuPath(node, vmid, "/config"), params, nil)
}

func (c *HTTPClient) task(ctx context.Context, method, path string, params url.Values) (string, error) {
	var upid string
	if err := c.do(ctx, method, path, params, &upid); err != nil {
		return "", err
	}
	return upid, nil
}

// Clone clones a template into a new VM.
func (c *HTTPClient) Clone(ctx context.Context, node string, templateID int, o CloneOptions) (string, error) {
	p := url.Values{
		"newid": {strconv.Itoa(o.NewID)},
		"full":  {boolParam(o.Full)},
	}
	setIf(p, "name", o.Name)
	setIf(p, "description", o.Description)
	setIf(p, "pool", o.Pool)
	if o.Full {
		setIf(p, "storage", o.Storage)
	}
	setIf(p, "target", o.Target)
	return c.task(ctx, http.MethodPost, qemuPath(node, templateID, "/clone"), p)
}

// Resize sets a disk to an absolute size like "20G".
func (c *HTTPClient) Resize(ctx context.Context, node string, vmid int, disk, size string) (string, error) {
	return c.task(ctx, http.MethodPut, qemuPath(node, vmid, "/resize"),
		url.Values{"disk": {disk}, "size": {size}})
}

// Start powers the VM on.
func (c *HTTPClient) Start(ctx context.Context, node string, vmid int) (string, error) {
	return c.task(ctx, http.MethodPost, qemuPath(node, vmid, "/status/start"), url.Values{})
}

// Shutdown asks the guest to shut down via ACPI or the guest agent.
func (c *HTTPClient) Shutdown(ctx context.Context, node string, vmid int, o ShutdownOptions) (string, error) {
	p := url.Values{"forceStop": {boolParam(o.ForceStop)}}
	if o.TimeoutSeconds > 0 {
		p.Set("timeout", strconv.Itoa(o.TimeoutSeconds))
	}
	return c.task(ctx, http.MethodPost, qemuPath(node, vmid, "/status/shutdown"), p)
}

// Stop powers the VM off without involving the guest.
func (c *HTTPClient) Stop(ctx context.Context, node string, vmid int) (string, error) {
	return c.task(ctx, http.MethodPost, qemuPath(node, vmid, "/status/stop"), url.Values{})
}

// Delete destroys the VM, its disks, and every reference to it (backup
// jobs, replication, HA) via purge.
func (c *HTTPClient) Delete(ctx context.Context, node string, vmid int) (string, error) {
	return c.task(ctx, http.MethodDelete, qemuPath(node, vmid, ""),
		url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}})
}

// TaskStatus polls one task. The node is taken from the UPID itself.
func (c *HTTPClient) TaskStatus(ctx context.Context, upid string) (TaskStatus, error) {
	node, err := NodeFromUPID(upid)
	if err != nil {
		return TaskStatus{}, err
	}
	var s struct {
		Status     string `json:"status"`
		ExitStatus string `json:"exitstatus"`
	}
	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(node), url.PathEscape(upid))
	if err := c.do(ctx, http.MethodGet, path, nil, &s); err != nil {
		return TaskStatus{}, err
	}
	return TaskStatus{Done: s.Status == "stopped", ExitStatus: s.ExitStatus}, nil
}

// Get returns the raw "data" of a GET, for debugging (hack/pvels).
func (c *HTTPClient) Get(ctx context.Context, path string, params url.Values) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodGet, path, params, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// RecentTasks lists the last tasks for one VM, finished ones included, as
// "type: status" lines. Debugging aid (hack/pvels); not part of Client.
func (c *HTTPClient) RecentTasks(ctx context.Context, node string, vmid, limit int) ([]string, error) {
	var items []struct {
		UPID   string `json:"upid"`
		Type   string `json:"type"`
		Status string `json:"status"`
	}
	path := fmt.Sprintf("/nodes/%s/tasks", url.PathEscape(node))
	params := url.Values{"vmid": {strconv.Itoa(vmid)}, "limit": {strconv.Itoa(limit)}}
	if err := c.do(ctx, http.MethodGet, path, params, &items); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		out = append(out, it.Type+": "+it.Status)
		if i == 0 && it.Status != "OK" {
			// The newest failed task: include its log, which carries the
			// real reason (e.g. QEMU's stderr).
			var lines []struct {
				T string `json:"t"`
			}
			logPath := fmt.Sprintf("/nodes/%s/tasks/%s/log", url.PathEscape(node), url.PathEscape(it.UPID))
			if err := c.do(ctx, http.MethodGet, logPath, url.Values{"limit": {"30"}}, &lines); err == nil {
				for _, l := range lines {
					out = append(out, "    | "+l.T)
				}
			}
		}
	}
	return out, nil
}

// ActiveTasks lists running tasks for one VM.
func (c *HTTPClient) ActiveTasks(ctx context.Context, node string, vmid int) ([]Task, error) {
	var items []struct {
		UPID   string `json:"upid"`
		Type   string `json:"type"`
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	path := fmt.Sprintf("/nodes/%s/tasks", url.PathEscape(node))
	params := url.Values{"vmid": {strconv.Itoa(vmid)}, "source": {"active"}}
	if err := c.do(ctx, http.MethodGet, path, params, &items); err != nil {
		return nil, err
	}
	var out []Task
	for _, it := range items {
		// PVE 9.2 reports a running task as "RUNNING" (upper case) in this
		// list; a task that just finished carries its exit status instead.
		if it.Status != "" && !strings.EqualFold(it.Status, "running") {
			continue
		}
		out = append(out, Task{UPID: it.UPID, Type: it.Type})
	}
	return out, nil
}

// AgentInterfaces asks the QEMU guest agent for the guest's NICs. Fails
// when the agent is not installed, not enabled, or not up yet.
func (c *HTTPClient) AgentInterfaces(ctx context.Context, node string, vmid int) ([]Interface, error) {
	var r struct {
		Result []struct {
			Name        string `json:"name"`
			MAC         string `json:"hardware-address"`
			IPAddresses []struct {
				Address string `json:"ip-address"`
				Type    string `json:"ip-address-type"`
				Prefix  int    `json:"prefix"`
			} `json:"ip-addresses"`
		} `json:"result"`
	}
	if err := c.do(ctx, http.MethodGet,
		qemuPath(node, vmid, "/agent/network-get-interfaces"), nil, &r); err != nil {
		return nil, err
	}
	out := make([]Interface, 0, len(r.Result))
	for _, n := range r.Result {
		ifc := Interface{Name: n.Name, MAC: strings.ToLower(n.MAC)}
		for _, a := range n.IPAddresses {
			ifc.IPs = append(ifc.IPs, IP{Address: a.Address, Type: a.Type, Prefix: a.Prefix})
		}
		out = append(out, ifc)
	}
	return out, nil
}

func boolParam(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func setIf(p url.Values, k, v string) {
	if v != "" {
		p.Set(k, v)
	}
}
