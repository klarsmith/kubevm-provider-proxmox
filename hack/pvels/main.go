// SPDX-License-Identifier: Apache-2.0

// Command pvels lists the QEMU VMs a credentials Secret can see, with the
// fields the provider relies on: pool, status, identity marker, running
// tasks. Read-only; for debugging against a test PVE.
//
// Usage: go run ./hack/pvels [path/to/credentials.yaml] [--get /api/path?query]
//
// --get prints the raw "data" of one GET, e.g. --get '/nodes/pve/tasks?source=active'.
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "pvels:", err)
		os.Exit(1)
	}
}

func run() error {
	path, rawGet := ".local/credentials.yaml", ""
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--get" && i+1 < len(os.Args) {
			rawGet = os.Args[i+1]
			i++
			continue
		}
		path = os.Args[i]
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var s corev1.Secret
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	px, err := proxmox.NewHTTPClient(proxmox.Credentials{
		URL:                s.StringData["url"],
		TokenID:            s.StringData["tokenID"],
		TokenSecret:        s.StringData["tokenSecret"],
		InsecureSkipVerify: s.StringData["insecureSkipVerify"] == "true",
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if rawGet != "" {
		p, q, _ := strings.Cut(rawGet, "?")
		params, err := url.ParseQuery(q)
		if err != nil {
			return err
		}
		raw, err := px.Get(ctx, p, params)
		if err != nil {
			return err
		}
		fmt.Println(string(raw))
		return nil
	}

	vms, err := px.Resources(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("%-6s %-8s %-12s %-9s %-10s %s\n", "VMID", "NODE", "NAME", "STATUS", "POOL", "MARKER / TASKS")
	for _, vm := range vms {
		marker := ""
		if !vm.Template {
			if cfg, err := px.Config(ctx, vm.Node, vm.VMID); err == nil {
				marker, _, _ = strings.Cut(cfg["description"], "\n")
				if cfg["lock"] != "" {
					marker += " lock=" + cfg["lock"]
				}
			} else {
				marker = "config: " + err.Error()
			}
			if tasks, err := px.ActiveTasks(ctx, vm.Node, vm.VMID); err == nil {
				for _, t := range tasks {
					marker += " task=" + t.Type
				}
			} else {
				marker += " tasks: " + err.Error()
			}
		} else {
			marker = "(template)"
			// Clones are filed under the template's VMID, so they show here.
			if tasks, err := px.ActiveTasks(ctx, vm.Node, vm.VMID); err == nil {
				for _, t := range tasks {
					marker += " task=" + t.Type
				}
			}
		}
		fmt.Printf("%-6d %-8s %-12s %-9s %-10s %s\n", vm.VMID, vm.Node, vm.Name, vm.Status, vm.Pool, marker)
		if !vm.Template {
			if recent, err := px.RecentTasks(ctx, vm.Node, vm.VMID, 6); err == nil {
				for _, t := range recent {
					fmt.Printf("       task  %s\n", t)
				}
			}
		}
	}
	return nil
}
