// SPDX-License-Identifier: Apache-2.0

// Package proxmox is the seam between the controller and the Proxmox VE API.
//
// Client is the only thing the controller calls. HTTPClient implements it
// against the real REST API with an API token; Fake implements it in memory
// so no test needs a reachable Proxmox.
package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ErrNotFound means the VM or task the call named does not exist.
var ErrNotFound = errors.New("not found")

// VM is one QEMU guest as listed by /cluster/resources.
type VM struct {
	VMID     int
	Node     string
	Name     string
	Pool     string
	Status   string // "running" or "stopped" ("paused" from CurrentStatus)
	Template bool
	// Container is set for LXC containers. They share the VMID space with
	// QEMU guests, so they matter when checking whether an ID is taken,
	// and are otherwise ignored.
	Container bool
}

// TaskStatus is the state of one asynchronous Proxmox task.
type TaskStatus struct {
	Done bool
	// ExitStatus is "OK" on success; anything else is the failure message.
	ExitStatus string
}

// Failed reports a finished task that did not succeed. Proxmox reports a
// task that completed with warnings as "WARNINGS: n", which is a success.
func (t TaskStatus) Failed() bool {
	return t.Done && t.ExitStatus != "OK" && !strings.HasPrefix(t.ExitStatus, "WARNINGS")
}

// CloneOptions are the parameters of POST /nodes/{node}/qemu/{vmid}/clone.
type CloneOptions struct {
	NewID       int
	Name        string
	Description string
	Pool        string
	Storage     string // full clones only
	Target      string // empty means the template's node
	Full        bool
}

// ShutdownOptions are the parameters of a guest shutdown.
type ShutdownOptions struct {
	TimeoutSeconds int
	// ForceStop stops the VM hard once the timeout passes.
	ForceStop bool
}

// IP is one guest address reported by the QEMU guest agent.
type IP struct {
	Address string
	Type    string // "ipv4" or "ipv6"
	Prefix  int
}

// Interface is one guest NIC reported by the QEMU guest agent.
type Interface struct {
	Name string
	MAC  string // lower-case, colon-separated
	IPs  []IP
}

// Client is every Proxmox call the controller makes.
//
// Calls that start a Proxmox task return its UPID; the caller polls
// TaskStatus rather than blocking. node is always the VM's CURRENT node,
// looked up from Resources, never a stored one.
type Client interface {
	Version(ctx context.Context) (string, error)
	ClusterName(ctx context.Context) (string, error)
	// Resources lists every QEMU guest. Its status field comes from a
	// cluster-wide cache that pvestatd refreshes every few seconds, so it can
	// lag the VM's real state; use CurrentStatus before acting on a VM.
	Resources(ctx context.Context) ([]VM, error)
	// CurrentStatus reads one VM's live state: "running", "stopped", or
	// "paused" (running but not executing: paused by a user, or by QEMU on
	// an I/O error such as full storage).
	CurrentStatus(ctx context.Context, node string, vmid int) (string, error)
	NextID(ctx context.Context) (int, error)
	Config(ctx context.Context, node string, vmid int) (map[string]string, error)
	SetConfig(ctx context.Context, node string, vmid int, params url.Values) error
	Clone(ctx context.Context, node string, templateID int, o CloneOptions) (string, error)
	// Resize returns "" on Proxmox versions where resize is synchronous.
	Resize(ctx context.Context, node string, vmid int, disk, size string) (string, error)
	Start(ctx context.Context, node string, vmid int) (string, error)
	Shutdown(ctx context.Context, node string, vmid int, o ShutdownOptions) (string, error)
	Stop(ctx context.Context, node string, vmid int) (string, error)
	Delete(ctx context.Context, node string, vmid int) (string, error)
	TaskStatus(ctx context.Context, upid string) (TaskStatus, error)
	// ActiveTasks lists the tasks still running for one VM. Used to adopt
	// a task this controller started but failed to record.
	ActiveTasks(ctx context.Context, node string, vmid int) ([]Task, error)
	AgentInterfaces(ctx context.Context, node string, vmid int) ([]Interface, error)
}

// Task is one running Proxmox task.
type Task struct {
	UPID string
	Type string // e.g. "qmstart", "qmshutdown", "qmstop", "qmclone", "qmdestroy", "qmresize"
}

// IsAlreadyExists reports a clone that failed because the new VMID is taken,
// the /cluster/nextid race.
func IsAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already exists")
}

// NodeFromUPID extracts the node name from a task UPID
// (UPID:<node>:<pid>:<pstart>:<starttime>:<type>:<id>:<user>:).
func NodeFromUPID(upid string) (string, error) {
	parts := strings.Split(upid, ":")
	if len(parts) < 3 || parts[0] != "UPID" || parts[1] == "" {
		return "", fmt.Errorf("malformed UPID %q", upid)
	}
	return parts[1], nil
}
