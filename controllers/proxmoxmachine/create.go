// SPDX-License-Identifier: Apache-2.0

package proxmoxmachine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
)

// markerPrefix starts the identity line written into a cloned VM's
// description. Proxmox VM names are not unique and tags cannot hold "=", so
// the ProxmoxMachine UID goes into the description, which the clone call
// writes atomically with the new VM's config.
const markerPrefix = "kubevm-uid="

func marker(m *pxv1a1.ProxmoxMachine) string {
	return markerPrefix + string(m.UID)
}

func description(m *pxv1a1.ProxmoxMachine) string {
	return fmt.Sprintf("%s\nManaged by kubevm-provider-proxmox for ProxmoxMachine %s/%s. "+
		"Do not edit the line above.", marker(m), m.Namespace, m.Name)
}

// vmName is the Proxmox VM name, which Proxmox cloud-init also uses as the
// guest hostname.
func vmName(m *pxv1a1.ProxmoxMachine) string {
	if m.Spec.HostName != "" {
		return m.Spec.HostName
	}
	return m.Name
}

// hasMarker reports whether a VM config carries this object's identity line.
func hasMarker(m *pxv1a1.ProxmoxMachine, cfg map[string]string) bool {
	want := marker(m)
	for _, line := range strings.Split(cfg["description"], "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// findAllByMarker returns every VM in the allowed pool whose description
// carries this object's UID. Only pool members are read, so a VM outside the
// pool can never be adopted or deleted.
func (r *Reconciler) findAllByMarker(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vms []proxmox.VM) ([]*proxmox.VM, error) {

	var out []*proxmox.VM
	for i := range vms {
		vm := &vms[i]
		if vm.Template || vm.Container || vm.Pool != r.AllowedPool {
			continue
		}
		cfg, err := px.Config(ctx, vm.Node, vm.VMID)
		if errors.Is(err, proxmox.ErrNotFound) {
			continue // deleted between the listing and now
		}
		if err != nil {
			return nil, fmt.Errorf("reading config of VM %d: %w", vm.VMID, err)
		}
		if hasMarker(m, cfg) {
			out = append(out, vm)
		}
	}
	return out, nil
}

// findByMarker returns the first VM findAllByMarker finds, or nil.
func (r *Reconciler) findByMarker(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vms []proxmox.VM) (*proxmox.VM, error) {

	all, err := r.findAllByMarker(ctx, m, px, vms)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[0], nil
}

// reserveVMID picks a VMID and records it in status, BEFORE anything is
// cloned. The clone then targets exactly that ID, and Proxmox refuses a
// second clone into an existing ID, so a retry after a lost status write can
// never create a second VM.
//
// Found on real PVE 9.2: a VM is not a member of its pool until its clone
// task finishes, so a marker lookup restricted to the pool cannot see a
// clone in flight. Recovering by lookup alone cloned the template twice.
func (r *Reconciler) reserveVMID(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, sw *statusWriter) error {

	newID, err := px.NextID(ctx)
	if err != nil {
		return fmt.Errorf("getting next VMID: %w", err)
	}
	m.Status.VMID = newID
	m.Status.PendingOp, m.Status.PendingTask = pxv1a1.OpClone, ""
	setCondition(m, notReady(pxv1a1.ReasonProvisioning, fmt.Sprintf("reserved VMID %d for the clone", newID)))
	// Written now, not with the rest of the status: nothing may be cloned
	// until this is durable.
	return sw.flush(ctx, m)
}

// create clones the template into the reserved VMID in the allowed pool.
func (r *Reconciler) create(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vms []proxmox.VM, sw *statusWriter) (ctrl.Result, error) {

	var tmpl *proxmox.VM
	matches := 0
	for i := range vms {
		if vms[i].Template && !vms[i].Container && vms[i].Name == m.Spec.Template {
			tmpl = &vms[i]
			matches++
		}
	}
	switch {
	case matches == 0:
		setCondition(m, notReady(pxv1a1.ReasonInvalidConfiguration, fmt.Sprintf(
			"no Proxmox template named %q (the VM must be converted to a template)", m.Spec.Template)))
		return ctrl.Result{RequeueAfter: failureRequeueDelay}, nil
	case matches > 1:
		setCondition(m, notReady(pxv1a1.ReasonInvalidConfiguration, fmt.Sprintf(
			"%d Proxmox templates are named %q; template names must be unique", matches, m.Spec.Template)))
		return ctrl.Result{RequeueAfter: failureRequeueDelay}, nil
	}

	if wait := r.waitRemaining(m); wait > 0 {
		setCondition(m, notReady(pxv1a1.ReasonTaskFailed, r.failureMessage(m)))
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	if m.Status.VMID == 0 {
		if err := r.reserveVMID(ctx, m, px, sw); err != nil {
			return ctrl.Result{}, err
		}
	}
	newID := m.Status.VMID

	opts := proxmox.CloneOptions{
		NewID:       newID,
		Name:        vmName(m),
		Description: description(m),
		Pool:        r.AllowedPool,
		Storage:     m.Spec.Storage,
		Full:        m.Spec.FullClone == nil || *m.Spec.FullClone,
	}
	if m.Spec.Node != "" && m.Spec.Node != tmpl.Node {
		opts.Target = m.Spec.Node
	}

	upid, err := px.Clone(ctx, tmpl.Node, tmpl.VMID, opts)
	if proxmox.IsAlreadyExists(err) {
		// Either our own earlier clone into this ID (its UPID was never
		// recorded) or another client's VM. resumeClone tells them apart
		// once the VM is visible.
		setCondition(m, notReady(pxv1a1.ReasonProvisioning,
			fmt.Sprintf("VM %d already exists but is not visible yet; waiting for it to show up as this machine's clone", newID)))
		return ctrl.Result{RequeueAfter: r.pollDelay()}, nil
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("cloning template %q (%d): %w", tmpl.Name, tmpl.VMID, err)
	}

	m.Status.PendingTask, m.Status.PendingOp = upid, pxv1a1.OpClone
	m.Status.ProviderMetadata = map[string]string{"template": m.Spec.Template, "vmid": fmt.Sprint(newID)}
	setCondition(m, notReady(pxv1a1.ReasonProvisioning,
		fmt.Sprintf("cloning template %q into VM %d", tmpl.Name, newID)))
	return ctrl.Result{RequeueAfter: r.pollDelay()}, nil
}

// releaseReservation gives up a reserved VMID that turned out to belong to
// someone else.
func releaseReservation(m *pxv1a1.ProxmoxMachine, msg string) {
	m.Status.VMID, m.Status.PendingOp, m.Status.PendingTask = 0, "", ""
	setCondition(m, notReady(pxv1a1.ReasonProvisioning, msg))
}

// containerHolds reports whether an LXC container uses the VMID.
func containerHolds(vms []proxmox.VM, vmid int) bool {
	for _, vm := range vms {
		if vm.Container && vm.VMID == vmid {
			return true
		}
	}
	return false
}

// resumeClone handles a reserved VMID whose clone UPID was never recorded:
// the clone was not sent yet, is still running, finished, or the ID went to
// someone else. done is true once the clone is known complete.
func (r *Reconciler) resumeClone(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vms []proxmox.VM, sw *statusWriter) (ctrl.Result, bool, error) {

	vm := byID(vms, m.Status.VMID) // any pool: a VM mid-clone has none yet
	if vm == nil {
		if containerHolds(vms, m.Status.VMID) {
			// An LXC container took the reserved ID: its "already exists"
			// would never clear.
			releaseReservation(m, fmt.Sprintf("VMID %d is taken by a container; picking a new one",
				m.Status.VMID))
			return ctrl.Result{Requeue: true}, false, nil
		}
		// Not sent yet, or not visible yet (a VM mid-clone is in no pool,
		// so a pool-scoped token may not see it): (re)send. A duplicate send
		// fails with "already exists" and is harmless. If the ID belongs to
		// a VM this token can never see, this waits and says so in the
		// condition; the reservation is never dropped on a timer, because
		// a slow clone looks exactly the same and dropping it would clone
		// twice.
		res, err := r.create(ctx, m, px, vms, sw)
		return res, false, err
	}

	cfg, err := px.Config(ctx, vm.Node, vm.VMID)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("reading config of VM %d: %w", vm.VMID, err)
	}
	// Lock first: PVE writes a placeholder config holding only the clone
	// lock, and the description (our marker) only lands later in the clone.
	// Until the lock clears, the VM's owner cannot be told.
	if cfg["lock"] == "clone" {
		setCondition(m, notReady(pxv1a1.ReasonTaskRunning,
			fmt.Sprintf("clone into VM %d is in progress", vm.VMID)))
		return ctrl.Result{RequeueAfter: r.pollDelay()}, false, nil
	}
	if !hasMarker(m, cfg) {
		// Another client took the reserved ID before our clone landed.
		releaseReservation(m, fmt.Sprintf(
			"VMID %d was taken by another VM before the clone landed; picking a new one", vm.VMID))
		return ctrl.Result{Requeue: true}, false, nil
	}
	m.Status.PendingOp = ""
	return ctrl.Result{}, true, nil
}
