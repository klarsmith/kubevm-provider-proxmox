// SPDX-License-Identifier: Apache-2.0

package proxmoxfake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Server serves the subset of the Proxmox VE REST API that HTTPClient
// uses, backed by a Fake. It exists so HTTPClient is tested over real HTTP,
// and so the whole manager can run on kind before a real PVE is available
// (see cmd/fakepve).
type Server struct {
	Fake *Fake
	// Token, if set, is the only accepted Authorization header value.
	Token string
}

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if s.Token != "" && req.Header.Get("Authorization") != s.Token {
		http.Error(w, "authentication failure", http.StatusUnauthorized)
		return
	}
	if err := req.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	data, err := s.route(req.Context(), req.Method, strings.TrimPrefix(req.URL.Path, "/api2/json"), req.Form)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errNoRoute) {
			status = http.StatusNotImplemented
		}
		// Proxmox reports errors in the status line, with a null body.
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

var errNoRoute = errors.New("not implemented by the fake")

func (s *Server) route(ctx context.Context, method, path string, form url.Values) (any, error) {
	f := s.Fake
	p := strings.Split(strings.Trim(path, "/"), "/")

	switch {
	case method == http.MethodGet && path == "/version":
		v, err := f.Version(ctx)
		return map[string]string{"version": v, "release": v[:strings.LastIndex(v, ".")]}, err

	case method == http.MethodGet && path == "/cluster/status":
		name, err := f.ClusterName(ctx)
		return []map[string]any{{"type": "cluster", "name": name}, {"type": "node", "name": "pve"}}, err

	case method == http.MethodGet && path == "/cluster/resources":
		vms, err := f.Resources(ctx)
		out := make([]map[string]any, 0, len(vms))
		for _, vm := range vms {
			typ := "qemu"
			if vm.Container {
				typ = "lxc"
			}
			out = append(out, map[string]any{
				"type": typ, "vmid": vm.VMID, "node": vm.Node, "name": vm.Name,
				"pool": vm.Pool, "status": vm.Status, "template": boolInt(vm.Template),
			})
		}
		return out, err

	case method == http.MethodGet && path == "/cluster/nextid":
		id, err := f.NextID(ctx)
		return strconv.Itoa(id), err

	case method == http.MethodGet && len(p) == 3 && p[0] == "nodes" && p[2] == "tasks":
		vmid, _ := strconv.Atoi(form.Get("vmid"))
		tasks, err := f.ActiveTasks(ctx, p[1], vmid)
		out := make([]map[string]any, 0, len(tasks))
		for _, t := range tasks {
			// As PVE 9.2 answers: upper-case status for a running task.
			out = append(out, map[string]any{
				"upid": t.UPID, "type": t.Type, "id": strconv.Itoa(vmid), "status": "RUNNING",
			})
		}
		return out, err

	case len(p) == 5 && p[0] == "nodes" && p[2] == "tasks" && p[4] == "status":
		upid, _ := url.PathUnescape(p[3])
		ts, err := f.TaskStatus(ctx, upid)
		status := "running"
		if ts.Done {
			status = "stopped"
		}
		return map[string]string{"status": status, "exitstatus": ts.ExitStatus}, err
	}

	// /nodes/{node}/qemu/{vmid}[/...]
	if len(p) < 4 || p[0] != "nodes" || p[2] != "qemu" {
		return nil, fmt.Errorf("%w: %s %s", errNoRoute, method, path)
	}
	node := p[1]
	vmid, err := strconv.Atoi(p[3])
	if err != nil {
		return nil, fmt.Errorf("bad vmid %q", p[3])
	}
	rest := strings.Join(p[4:], "/")

	switch {
	case method == http.MethodGet && rest == "status/current":
		st, err := f.CurrentStatus(ctx, node, vmid)
		return map[string]string{"status": st}, err
	case method == http.MethodGet && rest == "config":
		return f.Config(ctx, node, vmid)
	case method == http.MethodPut && rest == "config":
		params := url.Values{}
		for k, v := range form {
			params[k] = v
		}
		return nil, f.SetConfig(ctx, node, vmid, params)
	case method == http.MethodPost && rest == "clone":
		newID, _ := strconv.Atoi(form.Get("newid"))
		return f.Clone(ctx, node, vmid, proxmox.CloneOptions{
			NewID: newID, Name: form.Get("name"), Description: form.Get("description"),
			Pool: form.Get("pool"), Storage: form.Get("storage"), Target: form.Get("target"),
			Full: form.Get("full") == "1",
		})
	case method == http.MethodPut && rest == "resize":
		return f.Resize(ctx, node, vmid, form.Get("disk"), form.Get("size"))
	case method == http.MethodPost && rest == "status/start":
		return f.Start(ctx, node, vmid)
	case method == http.MethodPost && rest == "status/stop":
		return f.Stop(ctx, node, vmid)
	case method == http.MethodPost && rest == "status/shutdown":
		timeout, _ := strconv.Atoi(form.Get("timeout"))
		return f.Shutdown(ctx, node, vmid, proxmox.ShutdownOptions{
			TimeoutSeconds: timeout, ForceStop: form.Get("forceStop") == "1",
		})
	case method == http.MethodDelete && rest == "":
		if form.Get("purge") != "1" {
			return nil, errors.New("fake requires purge=1")
		}
		return f.Delete(ctx, node, vmid)
	case method == http.MethodGet && rest == "agent/network-get-interfaces":
		ifaces, err := f.AgentInterfaces(ctx, node, vmid)
		if err != nil {
			return nil, err
		}
		result := make([]map[string]any, 0, len(ifaces))
		for _, ifc := range ifaces {
			ips := make([]map[string]any, 0, len(ifc.IPs))
			for _, ip := range ifc.IPs {
				ips = append(ips, map[string]any{
					"ip-address": ip.Address, "ip-address-type": ip.Type, "prefix": ip.Prefix,
				})
			}
			result = append(result, map[string]any{
				"name": ifc.Name, "hardware-address": ifc.MAC, "ip-addresses": ips,
			})
		}
		return map[string]any{"result": result}, nil
	}
	return nil, fmt.Errorf("%w: %s %s", errNoRoute, method, path)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
