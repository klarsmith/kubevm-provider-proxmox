// SPDX-License-Identifier: Apache-2.0

package proxmox_test

import (
	"context"
	"errors"
	. "github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox/proxmoxfake"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

const testToken = "PVEAPIToken=kubevm@pve!ctl=s3cret"

func newTestClient(t *testing.T) (*HTTPClient, *proxmoxfake.Fake) {
	t.Helper()
	f := proxmoxfake.New()
	srv := httptest.NewServer(&proxmoxfake.Server{Fake: f, Token: testToken})
	t.Cleanup(srv.Close)
	c, err := NewHTTPClient(Credentials{URL: srv.URL, TokenID: "kubevm@pve!ctl", TokenSecret: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	return c, f
}

func wait(t *testing.T, c *HTTPClient, upid string) TaskStatus {
	t.Helper()
	for i := 0; i < 5; i++ {
		ts, err := c.TaskStatus(context.Background(), upid)
		if err != nil {
			t.Fatal(err)
		}
		if ts.Done {
			return ts
		}
	}
	t.Fatalf("task %s never finished", upid)
	return TaskStatus{}
}

func TestHTTPClientLifecycle(t *testing.T) {
	ctx := context.Background()
	c, f := newTestClient(t)

	if v, err := c.Version(ctx); err != nil || v != "8.2.4" {
		t.Fatalf("Version = %q, %v", v, err)
	}
	if n, err := c.ClusterName(ctx); err != nil || n != "testcluster" {
		t.Fatalf("ClusterName = %q, %v", n, err)
	}
	vms, err := c.Resources(ctx)
	if err != nil || len(vms) != 1 || !vms[0].Template || vms[0].VMID != 9000 {
		t.Fatalf("Resources = %+v, %v", vms, err)
	}
	id, err := c.NextID(ctx)
	if err != nil || id != 100 {
		t.Fatalf("NextID = %d, %v", id, err)
	}

	upid, err := c.Clone(ctx, "pve", 9000, CloneOptions{
		NewID: id, Name: "web-01", Description: "kubevm-uid=abc\nline two", Pool: "kubevm-dev",
		Storage: "local-lvm", Full: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ts := wait(t, c, upid); ts.Failed() {
		t.Fatalf("clone failed: %+v", ts)
	}
	cfg, err := c.Config(ctx, "pve", id)
	if err != nil || cfg["description"] != "kubevm-uid=abc\nline two" || cfg["cores"] != "1" {
		t.Fatalf("Config = %+v, %v", cfg, err)
	}

	keys := "ssh-ed25519%20AAA%2Bb%20a%40b"
	if err := c.SetConfig(ctx, "pve", id, url.Values{"cores": {"4"}, "sshkeys": {keys}}); err != nil {
		t.Fatal(err)
	}
	if vm, _ := f.Get(id); vm.Config["cores"] != "4" || vm.Config["sshkeys"] != keys {
		t.Errorf("SetConfig did not round-trip: cores=%q sshkeys=%q", vm.Config["cores"], vm.Config["sshkeys"])
	}

	upid, err = c.Start(ctx, "pve", id)
	if err != nil {
		t.Fatal(err)
	}
	active, err := c.ActiveTasks(ctx, "pve", id)
	if err != nil || len(active) != 1 || active[0].UPID != upid || active[0].Type != "qmstart" {
		t.Fatalf("ActiveTasks = %+v, %v; want the start task", active, err)
	}
	wait(t, c, upid)
	if active, err := c.ActiveTasks(ctx, "pve", id); err != nil || len(active) != 0 {
		t.Errorf("ActiveTasks after completion = %+v, %v", active, err)
	}
	ifaces, err := c.AgentInterfaces(ctx, "pve", id)
	if err != nil || len(ifaces) != 2 || ifaces[1].MAC != "bc:24:11:00:00:01" || ifaces[1].IPs[0].Address != "10.0.0.100" {
		t.Fatalf("AgentInterfaces = %+v, %v", ifaces, err)
	}

	upid, err = c.Shutdown(ctx, "pve", id, ShutdownOptions{TimeoutSeconds: 60, ForceStop: true})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, c, upid)
	upid, err = c.Delete(ctx, "pve", id)
	if err != nil {
		t.Fatal(err)
	}
	wait(t, c, upid)
	if _, err := c.Config(ctx, "pve", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("Config of deleted VM: err = %v, want ErrNotFound", err)
	}
}

func TestHTTPClientCloneConflict(t *testing.T) {
	c, f := newTestClient(t)
	f.AddVM(proxmoxfake.FakeVM{VM: VM{VMID: 100, Node: "pve", Name: "taken"}})
	_, err := c.Clone(context.Background(), "pve", 9000, CloneOptions{NewID: 100, Name: "x", Full: true})
	if !IsAlreadyExists(err) {
		t.Errorf("err = %v, want an already-exists error", err)
	}
}

func TestHTTPClientRejectsBadToken(t *testing.T) {
	srv := httptest.NewServer(&proxmoxfake.Server{Fake: proxmoxfake.New(), Token: testToken})
	defer srv.Close()
	c, err := NewHTTPClient(Credentials{URL: srv.URL, TokenID: "kubevm@pve!ctl", TokenSecret: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Version(context.Background())
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want an auth failure", err)
	}
}

func TestNewHTTPClientValidates(t *testing.T) {
	if _, err := NewHTTPClient(Credentials{URL: "https://x"}); err == nil {
		t.Error("accepted credentials without a token")
	}
	if _, err := NewHTTPClient(Credentials{URL: "https://x", TokenID: "a", TokenSecret: "b",
		CABundle: []byte("not pem")}); err == nil {
		t.Error("accepted a caBundle with no certificates")
	}
}

func TestNodeFromUPID(t *testing.T) {
	n, err := NodeFromUPID("UPID:pve2:000A1B2C:0123ABCD:66F00000:qmclone:100:root@pam:")
	if err != nil || n != "pve2" {
		t.Errorf("NodeFromUPID = %q, %v", n, err)
	}
	if _, err := NodeFromUPID("garbage"); err == nil {
		t.Error("accepted a malformed UPID")
	}
}

func TestTaskStatusFailed(t *testing.T) {
	for exit, failed := range map[string]bool{"OK": false, "WARNINGS: 2": false, "clone failed": true} {
		if got := (TaskStatus{Done: true, ExitStatus: exit}).Failed(); got != failed {
			t.Errorf("Failed(%q) = %v", exit, got)
		}
	}
	if (TaskStatus{}).Failed() {
		t.Error("a running task reported failed")
	}
}

var _ http.Handler = (*proxmoxfake.Server)(nil)
