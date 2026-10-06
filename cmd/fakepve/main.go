// SPDX-License-Identifier: Apache-2.0

// Command fakepve serves an in-memory Proxmox VE API for local development:
// run it, point a credentials Secret at it, and the real manager and real
// HTTP client run end to end on kind with no Proxmox. Not for production.
//
// It starts with one template, "debian-12" (VMID 9000), on node "pve".
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox/proxmoxfake"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8006", "Address to serve the fake API on (plain HTTP).")
	tokenID := flag.String("token-id", "kubevm@pve!dev", "Accepted API token ID.")
	secret := flag.String("token-secret", "dev", "Accepted API token secret.")
	polls := flag.Int("task-polls", 1, "Task status polls before a task finishes.")
	flag.Parse()

	f := proxmoxfake.New()
	f.TaskPolls = *polls
	srv := &http.Server{
		Addr:              *listen,
		Handler:           logRequests(&proxmoxfake.Server{Fake: f, Token: fmt.Sprintf("PVEAPIToken=%s=%s", *tokenID, *secret)}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("fake Proxmox VE API on http://%s/api2/json (token %s)", *listen, *tokenID)
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
