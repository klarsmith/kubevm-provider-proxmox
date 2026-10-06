// SPDX-License-Identifier: Apache-2.0

package proxmoxmachine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
)

// reconcileDelete hard-stops and destroys the VM, then releases the
// finalizer. A VM that is gone, or no longer in the allowed pool, is left
// alone and the finalizer released.
func (r *Reconciler) reconcileDelete(ctx context.Context, m *pxv1a1.ProxmoxMachine) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(m, pxv1a1.Finalizer) {
		return ctrl.Result{}, nil
	}

	if m.Spec.DeleteOnTermination != nil && !*m.Spec.DeleteOnTermination {
		// Keeping the disks would need them detached first, which is not
		// implemented. Block rather than destroy something asked to be kept.
		return ctrl.Result{}, r.markNotReady(ctx, m, pxv1a1.ReasonUnsupportedByProvider,
			"bootDisk.deleteOnTermination=false is not supported yet; the VM is kept and deletion "+
				"is blocked. Set it to true (or remove the finalizer by hand) to proceed")
	}

	px, err := r.proxmoxClient(ctx, m)
	if err != nil {
		return ctrl.Result{}, r.credentialsError(ctx, m, "cannot delete the VM: ", err)
	}

	sw := r.newStatusWriter(m)
	res, gone, derr := r.destroy(ctx, m, px, sw)
	if derr != nil {
		setCondition(m, notReady(pxv1a1.ReasonAPIError, derr.Error()))
	}
	if err := sw.flush(ctx, m); err != nil {
		return ctrl.Result{}, err
	}
	if derr != nil || !gone {
		return res, derr
	}

	base := m.DeepCopy()
	controllerutil.RemoveFinalizer(m, pxv1a1.Finalizer)
	// NotFound: a reconcile working from a stale cache already released it.
	return ctrl.Result{}, client.IgnoreNotFound(r.patchIfChanged(ctx, m, base))
}

// destroy moves the VM one step towards gone. gone is true once there is
// nothing left this controller is allowed to delete.
func (r *Reconciler) destroy(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, sw *statusWriter) (ctrl.Result, bool, error) {

	logger := log.FromContext(ctx)

	// Wait out whatever is running, including a clone: deleting a VM that
	// is still being cloned fails on the clone lock.
	if done, res, err := r.pollPendingTask(ctx, m, px); !done || err != nil {
		return res, false, err
	}

	vms, err := px.Resources(ctx)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("listing VMs: %w", err)
	}

	// A reserved clone whose task was never recorded may still be in
	// flight, and not yet visible in the pool. Settle it first, or the VM
	// it creates would outlive this object. Never send a clone from here.
	if m.Status.PendingOp == pxv1a1.OpClone && m.Status.PendingTask == "" {
		if res, wait, err := r.settleReservationForDelete(ctx, m, px, vms); err != nil || wait {
			return res, false, err
		}
	}

	var vm *proxmox.VM
	if m.Status.VMID != 0 {
		vm = byID(vms, m.Status.VMID)
		if vm != nil && vm.Pool != r.AllowedPool {
			logger.Info("VM left the allowed pool; not deleting it",
				"vmid", vm.VMID, "pool", vm.Pool, "allowedPool", r.AllowedPool)
			vm = nil
		}
	}
	if vm == nil {
		// The recorded VM is gone, or there never was one. Sweep any other
		// VM in the pool carrying this object's marker: one cloned without
		// its VMID being recorded, or a duplicate from an earlier bug.
		all, err := r.findAllByMarker(ctx, m, px, vms)
		if err != nil {
			return ctrl.Result{}, false, err
		}
		if len(all) == 0 {
			return ctrl.Result{}, true, nil
		}
		vm = all[0]
		m.Status.VMID = vm.VMID
	}

	// Live state, not the listing's cached one: acting on the cache sent
	// the same hard stop four times on a real PVE.
	if err := r.refreshLiveStatus(ctx, px, vm); errors.Is(err, proxmox.ErrNotFound) {
		return ctrl.Result{RequeueAfter: requeueSoon}, false, nil // gone; the next pass sweeps
	} else if err != nil {
		return ctrl.Result{}, false, err
	}

	if adopted, err := r.adoptActiveTask(ctx, m, px, vm); err != nil || adopted {
		return ctrl.Result{RequeueAfter: r.pollDelay()}, false, err
	}

	// Only a failed stop or destroy holds deletion back. A backoff left by,
	// say, a VM that could not start must not delay removing it.
	if wait := r.waitRemaining(m); wait > 0 && deletePathFailure(m) {
		setCondition(m, notReady(pxv1a1.ReasonDeleting, "waiting to retry: "+r.failureMessage(m)))
		return ctrl.Result{RequeueAfter: wait}, false, nil
	}

	var upid, op string
	if vm.Status == "running" {
		op = pxv1a1.OpStop
		upid, err = px.Stop(ctx, vm.Node, vm.VMID)
	} else {
		op = pxv1a1.OpDelete
		upid, err = px.Delete(ctx, vm.Node, vm.VMID)
	}
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("%s of VM %d: %w", op, vm.VMID, err)
	}
	m.Status.PendingTask, m.Status.PendingOp = upid, op
	m.Status.PowerState = ""
	setCondition(m, notReady(pxv1a1.ReasonDeleting, fmt.Sprintf("%s of VM %d in progress", op, vm.VMID)))
	return ctrl.Result{RequeueAfter: r.pollDelay()}, false, nil
}

func deletePathFailure(m *pxv1a1.ProxmoxMachine) bool {
	return strings.HasPrefix(m.Status.LastFailure, pxv1a1.OpStop+" ") ||
		strings.HasPrefix(m.Status.LastFailure, pxv1a1.OpDelete+" ")
}

// settleReservationForDelete resolves a reserved VMID whose clone task was
// never recorded, without ever sending a clone. wait is true while a clone
// may still land.
func (r *Reconciler) settleReservationForDelete(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vms []proxmox.VM) (ctrl.Result, bool, error) {

	if vm := byID(vms, m.Status.VMID); vm != nil {
		cfg, err := px.Config(ctx, vm.Node, vm.VMID)
		if err != nil && !errors.Is(err, proxmox.ErrNotFound) {
			return ctrl.Result{}, false, fmt.Errorf("reading config of VM %d: %w", vm.VMID, err)
		}
		if cfg["lock"] == "clone" {
			setCondition(m, notReady(pxv1a1.ReasonDeleting,
				fmt.Sprintf("waiting for the clone into VM %d to finish before deleting it", vm.VMID)))
			return ctrl.Result{RequeueAfter: r.pollDelay()}, true, nil
		}
		if !hasMarker(m, cfg) {
			// Someone else's VM took the reserved ID: not ours to delete.
			m.Status.VMID = 0
		}
		m.Status.PendingOp = ""
		return ctrl.Result{}, false, nil
	}

	// Not visible. A clone in flight is invisible to a pool-scoped token
	// (no pool until it finishes), but its task is: it runs under the
	// template's VMID, started by this token.
	inFlight, err := r.cloneInFlight(ctx, m, px, vms)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	if inFlight {
		setCondition(m, notReady(pxv1a1.ReasonDeleting, fmt.Sprintf(
			"a clone of template %q is running and may land as VM %d; waiting before deleting",
			m.Spec.Template, m.Status.VMID)))
		return ctrl.Result{RequeueAfter: r.pollDelay()}, true, nil
	}
	// The reservation never became a VM.
	m.Status.VMID, m.Status.PendingOp = 0, ""
	return ctrl.Result{}, false, nil
}

// cloneInFlight reports a running qmclone task on this machine's template.
// It cannot tell whose clone it is, so it errs on the side of waiting.
func (r *Reconciler) cloneInFlight(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vms []proxmox.VM) (bool, error) {

	for _, t := range vms {
		if !t.Template || t.Container || t.Name != m.Spec.Template {
			continue
		}
		tasks, err := px.ActiveTasks(ctx, t.Node, t.VMID)
		if err != nil {
			return false, fmt.Errorf("listing running tasks of template %d: %w", t.VMID, err)
		}
		for _, task := range tasks {
			if task.Type == "qmclone" {
				return true, nil
			}
		}
	}
	return false, nil
}
