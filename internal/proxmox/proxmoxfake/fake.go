// SPDX-License-Identifier: Apache-2.0

// Package proxmoxfake is an in-memory Proxmox VE for tests and local runs:
// Fake implements proxmox.Client directly, and Server serves it over the
// REST API for the real HTTP client. It is never linked into the manager.
package proxmoxfake

import (
	"context"
	"fmt"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Fake is an in-memory Proxmox for tests.
//
// It models what the controller depends on: VMs live in pools and on nodes,
// state-changing calls return a UPID, and their effect lands only once the
// task finishes, after TaskPolls TaskStatus calls. Hooks inject the failures
// the controller has to survive.
type Fake struct {
	mu sync.Mutex

	Cluster string
	PVE     string

	VMs     map[int]*FakeVM
	tasks   map[string]*fakeTask
	nextID  int
	taskSeq int

	// TaskPolls is how many TaskStatus calls a task stays running for.
	TaskPolls int

	// Calls records every mutating call, e.g. "clone 9000->100".
	Calls []string

	// TakeNextIDAsContainer makes TakeNextIDOnce's squatter an LXC
	// container instead of a VM.
	TakeNextIDAsContainer bool

	// CloneMarkerLate models PVE writing a placeholder config holding only
	// the clone lock, with name and description (the marker) landing when
	// the clone finishes.
	CloneMarkerLate bool

	// TakeNextIDOnce makes the next Clone fail as if another client took
	// the VMID first, the /cluster/nextid race.
	TakeNextIDOnce bool

	// FailTask makes the next task of this op ("clone", "shutdown", ...)
	// finish with this exit status.
	FailTask map[string]string

	// GuestRefusesShutdown makes shutdown without forceStop fail.
	GuestRefusesShutdown bool

	// AgentDown makes AgentInterfaces fail.
	AgentDown bool

	// ListedStatus overrides the status Resources reports for a VMID,
	// modelling the lag of PVE's cluster resource cache. CurrentStatus
	// always reports the live state.
	ListedStatus map[int]string

	// AlwaysFailTask makes every task of this op fail with this exit
	// status until removed (FailTask fails only the next one).
	AlwaysFailTask map[string]string

	// HiddenVMIDs are IDs taken by VMs the token cannot see: absent from
	// Resources, but a clone into them is refused with "already exists".
	HiddenVMIDs map[int]bool

	// ListedNode overrides the node Resources reports for a VMID (the
	// listing lagging behind a migration). Calls still go to the real node.
	ListedNode map[int]string

	// Err makes the named method ("Resources", "Clone", ...) fail.
	Err map[string]error
}

// FakeVM is one VM inside Fake.
type FakeVM struct {
	proxmox.VM
	Config     map[string]string
	Interfaces []proxmox.Interface
}

// base64Charset is the loose key-material check the fake applies: real PVE
// parses the key fully, the fake only refuses obvious garbage.
var base64Charset = regexp.MustCompile(`^[A-Za-z0-9+/=]+$`)

// fakeTask runs for a number of time steps (see step), then lands its effect.
type fakeTask struct {
	node   string
	vmid   int
	typ    string
	polls  int
	exit   string
	effect func()
	done   bool
}

var _ proxmox.Client = (*Fake)(nil)

// New returns a single-node cluster "pve" with one template.
func New() *Fake {
	f := &Fake{
		Cluster:        "testcluster",
		PVE:            "8.2.4",
		VMs:            map[int]*FakeVM{},
		tasks:          map[string]*fakeTask{},
		nextID:         100,
		TaskPolls:      1,
		FailTask:       map[string]string{},
		ListedStatus:   map[int]string{},
		AlwaysFailTask: map[string]string{},
		HiddenVMIDs:    map[int]bool{},
		ListedNode:     map[int]string{},
		Err:            map[string]error{},
	}
	f.AddVM(FakeVM{
		VM: proxmox.VM{VMID: 9000, Node: "pve", Name: "debian-12", Status: "stopped", Template: true},
		Config: map[string]string{
			"name": "debian-12", "template": "1", "cores": "1", "memory": "1024",
			"boot": "order=scsi0;net0", "scsi0": "local-lvm:base-9000-disk-0,size=4G",
			"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0", "agent": "1",
		},
	})
	return f
}

// AddVM inserts a VM directly, e.g. one created out-of-band.
func (f *Fake) AddVM(vm FakeVM) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm.Config == nil {
		vm.Config = map[string]string{}
	}
	cp := vm
	f.VMs[vm.VMID] = &cp
}

// Get returns a copy of a VM, or false.
func (f *Fake) Get(vmid int) (FakeVM, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.VMs[vmid]
	if !ok {
		return FakeVM{}, false
	}
	cp := *vm
	cp.Config = map[string]string{}
	for k, v := range vm.Config {
		cp.Config[k] = v
	}
	return cp, true
}

// Remove deletes a VM out-of-band.
func (f *Fake) Remove(vmid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.VMs, vmid)
}

// MoveVM relocates a VM to another node, as a migration does.
func (f *Fake) MoveVM(vmid int, node string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.VMs[vmid]; ok {
		vm.Node = node
	}
}

// FinishTask ends a task started with AddTask (or any task) on the next
// poll, with the given exit status ("OK" for success).
func (f *Fake) FinishTask(upid, exit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tasks[upid]; ok {
		t.polls = 0
		t.exit = exit
		if exit != "OK" {
			t.effect = nil
		}
	}
}

// SetStatus changes a VM's power state out-of-band.
func (f *Fake) SetStatus(vmid int, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm, ok := f.VMs[vmid]; ok {
		vm.Status = status
	}
}

// CallsWithPrefix returns the recorded calls starting with prefix.
func (f *Fake) CallsWithPrefix(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *Fake) err(method string) error {
	return f.Err[method]
}

func (f *Fake) vm(node string, vmid int) (*FakeVM, error) {
	vm, ok := f.VMs[vmid]
	if !ok {
		return nil, fmt.Errorf("%w: Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist",
			proxmox.ErrNotFound, node, vmid)
	}
	if vm.Node != node {
		return nil, fmt.Errorf("%w: Configuration file 'nodes/%s/qemu-server/%d.conf' does not exist", proxmox.ErrNotFound, node, vmid)
	}
	return vm, nil
}

func (f *Fake) newTask(node, op string, vmid int, effect func()) string {
	f.taskSeq++
	upid := fmt.Sprintf("UPID:%s:%08X:00000000:00000000:qm%s:%d:root@pam!test:", node, f.taskSeq, op, vmid)
	t := &fakeTask{node: node, vmid: vmid, typ: "qm" + op, polls: f.TaskPolls, exit: "OK", effect: effect}
	if exit, ok := f.FailTask[op]; ok {
		delete(f.FailTask, op)
		t.exit = exit
		t.effect = nil
	} else if exit, ok := f.AlwaysFailTask[op]; ok {
		t.exit = exit
		t.effect = nil
	}
	f.tasks[upid] = t
	return upid
}

// Version implements proxmox.Client.
func (f *Fake) Version(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.PVE, f.err("Version")
}

// ClusterName implements proxmox.Client.
func (f *Fake) ClusterName(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Cluster, f.err("ClusterName")
}

// Resources implements proxmox.Client.
func (f *Fake) Resources(context.Context) ([]proxmox.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err("Resources"); err != nil {
		return nil, err
	}
	for _, t := range f.tasks {
		t.step()
	}
	out := make([]proxmox.VM, 0, len(f.VMs))
	for _, vm := range f.VMs {
		listed := vm.VM
		if st, ok := f.ListedStatus[vm.VMID]; ok {
			listed.Status = st
		}
		if node, ok := f.ListedNode[vm.VMID]; ok {
			listed.Node = node
		}
		out = append(out, listed)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out, nil
}

// CurrentStatus implements proxmox.Client: the live state, ignoring ListedStatus.
func (f *Fake) CurrentStatus(_ context.Context, node string, vmid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err("CurrentStatus"); err != nil {
		return "", err
	}
	vm, err := f.vm(node, vmid)
	if err != nil {
		return "", err
	}
	return vm.Status, nil
}

// NextID implements proxmox.Client.
func (f *Fake) NextID(context.Context) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err("NextID"); err != nil {
		return 0, err
	}
	for {
		// Like PVE, skips every existing guest, visible to the token or not.
		if _, taken := f.VMs[f.nextID]; !taken && !f.HiddenVMIDs[f.nextID] {
			return f.nextID, nil
		}
		f.nextID++
	}
}

// Config implements proxmox.Client.
func (f *Fake) Config(_ context.Context, node string, vmid int) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err("Config"); err != nil {
		return nil, err
	}
	vm, err := f.vm(node, vmid)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(vm.Config))
	for k, v := range vm.Config {
		out[k] = v
	}
	return out, nil
}

// SetConfig implements proxmox.Client.
func (f *Fake) SetConfig(_ context.Context, node string, vmid int, params url.Values) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err("SetConfig"); err != nil {
		return err
	}
	vm, err := f.vm(node, vmid)
	if err != nil {
		return err
	}
	if enc := params.Get("sshkeys"); enc != "" {
		// PVE parses every key; one it cannot decode fails the whole call.
		dec, err := url.QueryUnescape(enc)
		if err != nil {
			return fmt.Errorf("%w: SSH public key validation error", proxmox.ErrInvalidParameter)
		}
		for _, line := range strings.Split(dec, "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || !strings.HasPrefix(fields[0], "ssh-") {
				return fmt.Errorf("%w: SSH public key validation error", proxmox.ErrInvalidParameter)
			}
			if !base64Charset.MatchString(fields[1]) {
				return fmt.Errorf("%w: SSH public key validation error", proxmox.ErrInvalidParameter)
			}
		}
	}
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
		vm.Config[k] = params.Get(k)
	}
	sort.Strings(keys)
	f.Calls = append(f.Calls, fmt.Sprintf("config %d %s", vmid, strings.Join(keys, ",")))
	return nil
}

// Clone implements proxmox.Client.
func (f *Fake) Clone(_ context.Context, node string, templateID int, o proxmox.CloneOptions) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err("Clone"); err != nil {
		return "", err
	}
	tmpl, err := f.vm(node, templateID)
	if err != nil {
		return "", err
	}
	if f.TakeNextIDOnce {
		f.TakeNextIDOnce = false
		f.VMs[o.NewID] = &FakeVM{
			VM: proxmox.VM{VMID: o.NewID, Node: node, Name: "someone-else", Status: "stopped",
				Container: f.TakeNextIDAsContainer},
			Config: map[string]string{"name": "someone-else"},
		}
	}
	if _, taken := f.VMs[o.NewID]; taken || f.HiddenVMIDs[o.NewID] {
		return "", fmt.Errorf("unable to create VM %d: config file already exists", o.NewID)
	}
	target := node
	if o.Target != "" {
		target = o.Target
	}
	cfg := map[string]string{}
	for k, v := range tmpl.Config {
		if k != "template" {
			cfg[k] = v
		}
	}
	cfg["name"] = o.Name
	cfg["description"] = o.Description
	cfg["lock"] = "clone"
	if f.CloneMarkerLate {
		cfg = map[string]string{"lock": "clone"}
	}
	final := map[string]string{}
	for k, v := range tmpl.Config {
		if k != "template" {
			final[k] = v
		}
	}
	final["name"], final["description"] = o.Name, o.Description
	// Proxmox writes the new VM's config (name, description, pool) as the
	// clone starts; only the disk copy happens inside the task.
	vm := &FakeVM{
		// Pool membership lands only when the clone finishes, as observed on
		// PVE 9.2: a VM mid-clone is listed with no pool.
		VM:     proxmox.VM{VMID: o.NewID, Node: target, Name: o.Name, Status: "stopped"},
		Config: cfg,
		Interfaces: []proxmox.Interface{
			{Name: "lo", IPs: []proxmox.IP{{Address: "127.0.0.1", Type: "ipv4", Prefix: 8}}},
			{Name: "eth0", MAC: "bc:24:11:00:00:01", IPs: []proxmox.IP{
				{Address: fmt.Sprintf("10.0.0.%d", o.NewID%250), Type: "ipv4", Prefix: 24},
				{Address: "fe80::1", Type: "ipv6", Prefix: 64},
			}},
		},
	}
	f.VMs[o.NewID] = vm
	f.Calls = append(f.Calls, fmt.Sprintf("clone %d->%d", templateID, o.NewID))
	// PVE files the qmclone task under the SOURCE VMID.
	upid := f.newTask(node, "clone", templateID, func() {
		vm.Config = final
		vm.Pool = o.Pool
	})
	if t := f.tasks[upid]; t.exit != "OK" {
		// PVE removes the partial VM of a failed clone.
		t.effect = func() { delete(f.VMs, o.NewID) }
	}
	return upid, nil
}

// AddTask starts a task that runs until FinishTask, e.g. a console
// (vncproxy) or a backup (vzdump) someone else opened on a VM.
func (f *Fake) AddTask(node string, vmid int, typ string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taskSeq++
	upid := fmt.Sprintf("UPID:%s:%08X:00000000:00000000:%s:%d:root@pam:", node, f.taskSeq, typ, vmid)
	f.tasks[upid] = &fakeTask{node: node, vmid: vmid, typ: typ, polls: 1 << 30, exit: "OK"}
	return upid
}

// Resize implements proxmox.Client.
func (f *Fake) Resize(_ context.Context, node string, vmid int, disk, size string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err("Resize"); err != nil {
		return "", err
	}
	vm, err := f.vm(node, vmid)
	if err != nil {
		return "", err
	}
	f.Calls = append(f.Calls, fmt.Sprintf("resize %d %s %s", vmid, disk, size))
	return f.newTask(node, "resize", vmid, func() {
		parts := strings.Split(vm.Config[disk], ",")
		for i, p := range parts {
			if strings.HasPrefix(p, "size=") {
				parts[i] = "size=" + size
			}
		}
		vm.Config[disk] = strings.Join(parts, ",")
	}), nil
}

func (f *Fake) power(op, node string, vmid int, to string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err(strings.ToUpper(op[:1]) + op[1:]); err != nil {
		return "", err
	}
	vm, err := f.vm(node, vmid)
	if err != nil {
		return "", err
	}
	f.Calls = append(f.Calls, fmt.Sprintf("%s %d", op, vmid))
	return f.newTask(node, op, vmid, func() { vm.Status = to }), nil
}

// Start implements proxmox.Client.
func (f *Fake) Start(_ context.Context, node string, vmid int) (string, error) {
	return f.power("start", node, vmid, "running")
}

// Shutdown implements proxmox.Client.
func (f *Fake) Shutdown(_ context.Context, node string, vmid int, o proxmox.ShutdownOptions) (string, error) {
	f.mu.Lock()
	if f.GuestRefusesShutdown && !o.ForceStop {
		f.FailTask["shutdown"] = "VM quit/powerdown failed"
	}
	f.mu.Unlock()
	return f.power("shutdown", node, vmid, "stopped")
}

// Stop implements proxmox.Client.
func (f *Fake) Stop(_ context.Context, node string, vmid int) (string, error) {
	return f.power("stop", node, vmid, "stopped")
}

// Delete implements proxmox.Client. Like Proxmox, it refuses a running VM.
func (f *Fake) Delete(_ context.Context, node string, vmid int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err("Delete"); err != nil {
		return "", err
	}
	vm, err := f.vm(node, vmid)
	if err != nil {
		return "", err
	}
	if vm.Status == "running" {
		return "", fmt.Errorf("VM %d is running - destroy failed", vmid)
	}
	f.Calls = append(f.Calls, fmt.Sprintf("delete %d", vmid))
	return f.newTask(node, "destroy", vmid, func() { delete(f.VMs, vmid) }), nil
}

// TaskStatus implements proxmox.Client.
func (f *Fake) TaskStatus(_ context.Context, upid string) (proxmox.TaskStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err("TaskStatus"); err != nil {
		return proxmox.TaskStatus{}, err
	}
	t, ok := f.tasks[upid]
	if !ok {
		return proxmox.TaskStatus{}, fmt.Errorf("%w: no such task %s", proxmox.ErrNotFound, upid)
	}
	t.step()
	if !t.done {
		return proxmox.TaskStatus{}, nil
	}
	return proxmox.TaskStatus{Done: true, ExitStatus: t.exit}, nil
}

// step advances a task by one unit of time. Polling it and listing VMs both
// count, so a task also finishes when nobody polls it, as real tasks do.
func (t *fakeTask) step() {
	if t.done {
		return
	}
	if t.polls > 0 {
		t.polls--
		return
	}
	if t.effect != nil {
		t.effect()
		t.effect = nil
	}
	t.done = true
}

// ActiveTasks implements proxmox.Client.
func (f *Fake) ActiveTasks(_ context.Context, node string, vmid int) ([]proxmox.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.err("ActiveTasks"); err != nil {
		return nil, err
	}
	var out []proxmox.Task
	for upid, t := range f.tasks {
		if !t.done && t.node == node && t.vmid == vmid {
			out = append(out, proxmox.Task{UPID: upid, Type: t.typ})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UPID < out[j].UPID })
	return out, nil
}

// AgentInterfaces implements proxmox.Client.
func (f *Fake) AgentInterfaces(_ context.Context, node string, vmid int) ([]proxmox.Interface, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, err := f.vm(node, vmid)
	if err != nil {
		return nil, err
	}
	if f.AgentDown || vm.Status != "running" {
		return nil, fmt.Errorf("QEMU guest agent is not running")
	}
	return vm.Interfaces, nil
}
