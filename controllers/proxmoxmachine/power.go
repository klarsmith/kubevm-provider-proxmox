// SPDX-License-Identifier: Apache-2.0

package proxmoxmachine

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	kubevmv1a1 "github.com/vmware-tanzu/vm-operator/external/kubevm/api/v1alpha1"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
)

const (
	powerOn  = kubevmv1a1.PowerStateOn
	powerOff = kubevmv1a1.PowerStateOff

	// shutdownTimeoutSeconds is how long a guest gets to shut down before
	// Soft fails or TrySoft falls back to a hard stop.
	shutdownTimeoutSeconds = 180
)

// applyPower starts a power task if the VM's state differs from the spec.
// issued is true when a task was started; the caller stops there and polls.
//
// PowerOffMode mapping:
//   - Hard    -> stop
//   - Soft    -> shutdown without forceStop; a guest that does not comply
//     fails the task, which is reported (Soft means "fail if the guest does
//     not shut down")
//   - TrySoft -> shutdown with forceStop, which Proxmox turns into a hard
//     stop once the timeout passes
func (r *Reconciler) applyPower(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vm *proxmox.VM) (ctrl.Result, bool, error) {

	var (
		upid string
		op   string
		err  error
	)
	switch kubevmv1a1.PowerState(m.Spec.PowerState) {
	case powerOn:
		if vm.Status != "stopped" {
			return ctrl.Result{}, false, nil
		}
		op = pxv1a1.OpStart
		upid, err = px.Start(ctx, vm.Node, vm.VMID)

	case powerOff:
		if vm.Status != "running" {
			return ctrl.Result{}, false, nil
		}
		switch kubevmv1a1.PowerOpMode(m.Spec.PowerOffMode) {
		case kubevmv1a1.PowerOpModeHard:
			op = pxv1a1.OpStop
			upid, err = px.Stop(ctx, vm.Node, vm.VMID)
		case kubevmv1a1.PowerOpModeSoft:
			op = pxv1a1.OpShutdown
			upid, err = px.Shutdown(ctx, vm.Node, vm.VMID,
				proxmox.ShutdownOptions{TimeoutSeconds: shutdownTimeoutSeconds})
		default: // TrySoft, and the API default when unset
			op = pxv1a1.OpShutdown
			upid, err = px.Shutdown(ctx, vm.Node, vm.VMID,
				proxmox.ShutdownOptions{TimeoutSeconds: shutdownTimeoutSeconds, ForceStop: true})
		}

	default:
		// Suspended (or unset): reported by UpToDate, not acted on.
		return ctrl.Result{}, false, nil
	}

	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("%s of VM %d: %w", op, vm.VMID, err)
	}
	m.Status.PendingTask, m.Status.PendingOp = upid, op
	m.Status.PowerState = ""
	setCondition(m, notReady(pxv1a1.ReasonPowerChanging,
		fmt.Sprintf("%s of VM %d in progress", op, vm.VMID)))
	return ctrl.Result{RequeueAfter: r.pollDelay()}, true, nil
}
