// SPDX-License-Identifier: Apache-2.0

package proxmoxmachine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
)

// converge moves the VM one step towards the spec and records what it
// observed in m.Status. The caller writes status once, afterwards.
//
// Order: finish any pending task, find (or adopt, or create) the VM, refuse
// anything outside the allowed pool, provision before first boot, apply
// power, then observe.
func (r *Reconciler) converge(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, unsupportedFields []string, sw *statusWriter) (ctrl.Result, error) {

	if done, res, err := r.pollPendingTask(ctx, m, px); !done || err != nil {
		return res, err
	}

	vms, err := px.Resources(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing VMs: %w", err)
	}

	if m.Status.VMID == 0 {
		found, err := r.findByMarker(ctx, m, px, vms)
		if err != nil {
			return ctrl.Result{}, err
		}
		if found == nil {
			return r.create(ctx, m, px, vms, sw)
		}
		// Adopt: a previous reconcile cloned it but never recorded the VMID.
		m.Status.VMID = found.VMID
	}

	// A reserved VMID with no recorded clone task: the clone may not have
	// been sent, may be running, or may be done. Settled before the pool
	// check, because a VM mid-clone is not in its pool yet.
	if m.Status.PendingOp == pxv1a1.OpClone && m.Status.PendingTask == "" {
		if res, done, err := r.resumeClone(ctx, m, px, vms, sw); err != nil || !done {
			return res, err
		}
	}

	vm := byID(vms, m.Status.VMID)
	if vm == nil {
		return r.vanished(m)
	}
	if vm.Pool != r.AllowedPool {
		m.Status.PowerState = ""
		setCondition(m, notReady(pxv1a1.ReasonOutsidePool, fmt.Sprintf(
			"VM %d is in pool %q, not %q; refusing to act on it", vm.VMID, vm.Pool, r.AllowedPool)))
		// Re-checked periodically: moving it back into the pool resumes.
		return ctrl.Result{RequeueAfter: resyncPeriod}, nil
	}

	// Decide on the VM's live state. The listing's status comes from a
	// cache that lags by several seconds; trusting it reported a VM that was
	// crash-looping on start as Ready, and repeated stops on delete.
	if err := r.refreshLiveStatus(ctx, px, vm); errors.Is(err, proxmox.ErrNotFound) {
		return r.vanished(m)
	} else if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.recordIdentity(ctx, m, px, vm); err != nil {
		return ctrl.Result{}, err
	}

	if adopted, err := r.adoptActiveTask(ctx, m, px, vm); err != nil || adopted {
		return ctrl.Result{RequeueAfter: r.pollDelay()}, err
	}

	notes := append([]string(nil), unsupportedFields...)

	// After a failed task, start nothing new until NextAttempt; observe
	// and report meanwhile.
	if wait := r.waitRemaining(m); wait > 0 {
		res, err := r.observe(ctx, m, px, vm, notes)
		r.reportBackoff(m, vm)
		return ctrl.Result{RequeueAfter: min(wait, res.RequeueAfter)}, err
	}

	if !m.Status.Provisioned {
		if vm.Status == "running" {
			// Adopted VM someone already booted: too late for pre-boot setup.
			m.Status.Provisioned = true
		} else {
			res, done, err := r.provision(ctx, m, px, vm)
			if err != nil || !done {
				return res, err
			}
		}
	}

	if res, issued, err := r.applyPower(ctx, m, px, vm); err != nil || issued {
		return res, err
	}
	drift, res, err := r.applyDrift(ctx, m, px, vm)
	if err != nil || m.Status.PendingTask != "" {
		return res, err
	}
	notes = append(notes, drift...)

	return r.observe(ctx, m, px, vm, notes)
}

// pollPendingTask checks the task recorded in status. done is false while
// it is still running.
func (r *Reconciler) pollPendingTask(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client) (done bool, res ctrl.Result, err error) {

	upid := m.Status.PendingTask
	if upid == "" {
		return true, ctrl.Result{}, nil
	}
	op := m.Status.PendingOp

	ts, err := px.TaskStatus(ctx, upid)
	if errors.Is(err, proxmox.ErrNotFound) {
		// The task record is gone (task log rotated, node reinstalled).
		// Forget it and look at the VM itself; a clone goes back to
		// "reserved, not sent", which resumeClone settles.
		m.Status.PendingTask = ""
		if op != pxv1a1.OpClone {
			m.Status.PendingOp = ""
		}
		return false, ctrl.Result{Requeue: true}, nil
	}
	if err != nil {
		return false, ctrl.Result{}, fmt.Errorf("polling %s task: %w", op, err)
	}
	if !ts.Done {
		if isPowerOp(op) {
			m.Status.PowerState = ""
		}
		setCondition(m, notReady(pxv1a1.ReasonTaskRunning, fmt.Sprintf("%s task %s is running", op, upid)))
		return false, ctrl.Result{RequeueAfter: r.pollDelay()}, nil
	}

	m.Status.PendingTask, m.Status.PendingOp = "", ""
	if ts.Failed() && op == pxv1a1.OpClone {
		// PVE removes a failed clone's partial VM. Keep the reserved ID and
		// go back to "reserved, not sent": resumeClone re-sends after the
		// backoff, or sees a leftover VM and waits on its lock or marker.
		// Reporting the VM as gone would be wrong: it never existed.
		m.Status.PendingOp = pxv1a1.OpClone
	}
	if ts.Failed() {
		// Reported and retried with exponential backoff. For a Soft
		// power-off this is the guest declining; the KubeVM contract says
		// that fails, so it is never escalated to a hard stop.
		r.recordTaskFailure(m, fmt.Sprintf("%s task failed: %s", op, ts.ExitStatus))
		setCondition(m, notReady(pxv1a1.ReasonTaskFailed, r.failureMessage(m)))
		setCondition(m, upToDate(false, pxv1a1.ReasonTaskFailed, r.failureMessage(m)))
		return false, ctrl.Result{RequeueAfter: r.waitRemaining(m)}, nil
	}
	recordTaskSuccess(m)
	return true, ctrl.Result{}, nil
}

// refreshLiveStatus replaces the listing's cached status with the VM's live
// state.
func (r *Reconciler) refreshLiveStatus(ctx context.Context, px proxmox.Client, vm *proxmox.VM) error {
	live, err := px.CurrentStatus(ctx, vm.Node, vm.VMID)
	if err != nil {
		return fmt.Errorf("reading live status of VM %d: %w", vm.VMID, err)
	}
	vm.Status = live
	return nil
}

// reportBackoff overrides the conditions observe set while retries are on
// hold: UpToDate is False (the spec is not applied), and
// InfrastructureReady is False when the VM is not in the asked-for power
// state, so a VM that cannot start never reads as Ready.
func (r *Reconciler) reportBackoff(m *pxv1a1.ProxmoxMachine, vm *proxmox.VM) {
	msg := r.failureMessage(m)
	setCondition(m, upToDate(false, pxv1a1.ReasonTaskFailed, msg))
	want := m.Spec.PowerState
	if (want == string(powerOn) && vm.Status != "running") ||
		(want == string(powerOff) && vm.Status != "stopped") {
		setCondition(m, notReady(pxv1a1.ReasonTaskFailed, msg))
	}
}

// adoptActiveTask records a task already running on the VM instead of
// starting another. That task is usually one this controller started and
// failed to record: the Proxmox call succeeded but the status write that
// would have saved its UPID lost an optimistic-lock race, or the manager
// restarted in between. It can also be someone else's (a migration from the
// web UI), which is equally a reason to wait. Proxmox VMs take one task at a
// time, so a second power call would fail on the VM lock anyway.
func (r *Reconciler) adoptActiveTask(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vm *proxmox.VM) (bool, error) {

	tasks, err := px.ActiveTasks(ctx, vm.Node, vm.VMID)
	if err != nil {
		return false, fmt.Errorf("listing running tasks of VM %d: %w", vm.VMID, err)
	}
	var t *proxmox.Task
	for i := range tasks {
		if lifecycleTaskTypes[tasks[i].Type] {
			t = &tasks[i]
			break
		}
	}
	if t == nil {
		// Only consoles (vncproxy, termproxy), backups and the like: they
		// do not change the VM's state, so they neither block nor make it
		// NotReady. A conflicting lock fails our task, which backs off.
		return false, nil
	}
	m.Status.PendingTask, m.Status.PendingOp = t.UPID, opForTaskType(t.Type)
	if isPowerOp(m.Status.PendingOp) {
		m.Status.PowerState = ""
	}
	setCondition(m, notReady(pxv1a1.ReasonTaskRunning,
		fmt.Sprintf("waiting for running %s task %s on VM %d", t.Type, t.UPID, vm.VMID)))
	return true, nil
}

// lifecycleTaskTypes are the tasks worth waiting for: ones this controller
// starts, plus migrations and disk moves, which change where the VM lives.
var lifecycleTaskTypes = map[string]bool{
	"qmstart": true, "qmshutdown": true, "qmstop": true, "qmreboot": true,
	"qmclone": true, "qmdestroy": true, "qmresize": true, "resize": true,
	"qmigrate": true, "qmmove": true,
}

func opForTaskType(typ string) string {
	switch typ {
	case "qmstart":
		return pxv1a1.OpStart
	case "qmshutdown":
		return pxv1a1.OpShutdown
	case "qmstop":
		return pxv1a1.OpStop
	case "qmclone":
		return pxv1a1.OpClone
	case "qmdestroy":
		return pxv1a1.OpDelete
	case "qmresize", "resize":
		return pxv1a1.OpResize
	}
	return typ // someone else's task, e.g. qmigrate: wait for it all the same
}

func isPowerOp(op string) bool {
	return op == pxv1a1.OpStart || op == pxv1a1.OpShutdown || op == pxv1a1.OpStop
}

func byID(vms []proxmox.VM, vmid int) *proxmox.VM {
	for i := range vms {
		if vms[i].VMID == vmid && !vms[i].Template && !vms[i].Container {
			return &vms[i]
		}
	}
	return nil
}

// vanished reports a VM that existed and is now gone. Terminal: recreating
// it silently would hide whatever removed it.
func (r *Reconciler) vanished(m *pxv1a1.ProxmoxMachine) (ctrl.Result, error) {
	m.Status.PowerState = ""
	m.Status.Addresses = nil
	setCondition(m, notReady(pxv1a1.ReasonVMGone, fmt.Sprintf(
		"VM %d no longer exists in Proxmox; delete and recreate the VirtualMachine to get a new one",
		m.Status.VMID)))
	return ctrl.Result{RequeueAfter: resyncPeriod}, nil
}

// recordIdentity fills the contract's providerID and providerMetadata.
func (r *Reconciler) recordIdentity(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vm *proxmox.VM) error {

	cluster, err := px.ClusterName(ctx)
	if err != nil {
		return fmt.Errorf("reading cluster name: %w", err)
	}
	version, err := px.Version(ctx)
	if err != nil {
		return fmt.Errorf("reading PVE version: %w", err)
	}
	m.Status.ProviderID = fmt.Sprintf("proxmox://%s/%d", cluster, vm.VMID)
	m.Status.ProviderMetadata = map[string]string{
		"node":       vm.Node,
		"vmid":       strconv.Itoa(vm.VMID),
		"template":   m.Spec.Template,
		"pool":       vm.Pool,
		"pveVersion": version,
	}
	return nil
}

// observe records power state, addresses and conditions for a VM with no
// operation in flight.
func (r *Reconciler) observe(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vm *proxmox.VM, notes []string) (ctrl.Result, error) {

	res := ctrl.Result{RequeueAfter: resyncPeriod}

	switch vm.Status {
	case "running":
		m.Status.PowerState = string(powerOn)
	case "stopped":
		m.Status.PowerState = string(powerOff)
	default:
		m.Status.PowerState = ""
	}

	m.Status.Addresses = nil
	if vm.Status == "running" {
		addrs, err := r.addresses(ctx, m, px, vm)
		if err != nil {
			notes = append(notes, "guest agent not answering, so no addresses yet: "+err.Error())
			res.RequeueAfter = r.pollDelay()
		} else {
			m.Status.Addresses = addrs
			if len(addrs) == 0 {
				res.RequeueAfter = r.pollDelay()
			}
		}
	}

	switch vm.Status {
	case "running":
		setCondition(m, ready(pxv1a1.ReasonRunning, fmt.Sprintf("VM %d is running on %s", vm.VMID, vm.Node)))
	case "stopped":
		setCondition(m, ready(pxv1a1.ReasonStopped, fmt.Sprintf("VM %d is stopped", vm.VMID)))
	case "paused":
		// Not acted on: a VM QEMU paused on an I/O error (full storage)
		// needs the cause fixed first, and resuming blindly would hide it.
		setCondition(m, notReady(pxv1a1.ReasonPaused, fmt.Sprintf(
			"VM %d is paused (by a user, or by QEMU on an I/O error such as full storage); "+
				"resume it in Proxmox once the cause is fixed", vm.VMID)))
	default:
		setCondition(m, notReady(pxv1a1.ReasonPowerChanging, fmt.Sprintf("VM %d is %s", vm.VMID, vm.Status)))
	}

	if hasUnsupported(notes) {
		setCondition(m, upToDate(false, pxv1a1.ReasonUnsupportedByProvider, strings.Join(notes, "; ")))
	} else {
		setCondition(m, upToDate(true, pxv1a1.ReasonApplied, strings.Join(notes, "; ")))
	}
	return res, nil
}

// hasUnsupported tells informational notes (agent down) from real gaps.
func hasUnsupported(notes []string) bool {
	for _, n := range notes {
		if !strings.HasPrefix(n, "guest agent") {
			return true
		}
	}
	return false
}
