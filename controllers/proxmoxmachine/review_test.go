// SPDX-License-Identifier: Apache-2.0

package proxmoxmachine

// Regression tests for failure modes found in review before the first
// commit. Each describes the scenario it guards against.

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	kubevmv1a1 "github.com/vmware-tanzu/vm-operator/external/kubevm/api/v1alpha1"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
)

// A reconcile working from a stale copy (status.vmid still 0) must not
// overwrite a newer status through the conflict retry: that is how a second
// VMID would get reserved and a second VM cloned.
func TestStatusWriterDoesNotOverwriteNewerStatus(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	stale := e.get() // status.vmid == 0

	fresh := e.get()
	fresh.Status.VMID = 100
	fresh.Status.PendingOp, fresh.Status.PendingTask = pxv1a1.OpClone, "UPID:pve:1:0:0:qmclone:9000:x:"
	if err := e.c.Status().Update(ctx, fresh); err != nil {
		t.Fatal(err)
	}

	sw := e.r.newStatusWriter(stale)
	stale.Status.VMID = 101
	stale.Status.PendingOp = pxv1a1.OpClone
	if err := sw.flush(ctx, stale); !apierrors.IsConflict(err) {
		t.Fatalf("flush err = %v, want a conflict", err)
	}
	if got := e.get().Status; got.VMID != 100 || got.PendingTask == "" {
		t.Errorf("newer status overwritten: %+v", got)
	}
}

// PVE writes a placeholder config holding only the clone lock; the marker
// lands later. Seeing such a VM at the reserved ID must mean "wait", not
// "someone else took it".
func TestCloneWithLateMarkerIsNotMistakenForAnotherVM(t *testing.T) {
	e := newEnv(t)
	e.px.CloneMarkerLate = true
	e.px.TaskPolls = 3
	lost := false
	e.failStatusPatch = func(m *pxv1a1.ProxmoxMachine) error {
		if !lost && m.Status.PendingOp == pxv1a1.OpClone && m.Status.PendingTask != "" {
			lost = true
			return apierrors.NewServiceUnavailable("etcdserver: request timed out")
		}
		return nil
	}
	m := e.until("running", 30, isRunningWithAddress)
	if !lost {
		t.Fatal("the clone's status write was not intercepted")
	}
	if n := len(e.px.CallsWithPrefix("clone")); n != 1 || m.Status.VMID != firstID {
		t.Errorf("%d clones, vmid %d; want 1 clone into %d", n, m.Status.VMID, firstID)
	}
}

// Delete must never send a clone. A reservation that never became a VM,
// with no clone running and the template gone, releases the finalizer.
func TestDeleteNeverClonesAndReleasesDeadReservation(t *testing.T) {
	m := machine()
	m.Finalizers = []string{pxv1a1.Finalizer}
	e := newEnv(t, parentVM(), m, secret())
	pm := e.get()
	pm.Status.VMID = firstID
	pm.Status.PendingOp = pxv1a1.OpClone
	if err := e.c.Status().Update(context.Background(), pm); err != nil {
		t.Fatal(err)
	}
	e.px.Remove(9000) // the template is gone too

	e.deleteMachine()
	e.until("deleted", 10, func(m *pxv1a1.ProxmoxMachine) bool { return m == nil })
	if got := e.px.CallsWithPrefix("clone"); got != nil {
		t.Errorf("delete sent a clone: %v", got)
	}
}

// A failed clone leaves no VM (PVE removes it). That is not "VM gone":
// the reserved ID is kept and the clone re-sent after the backoff.
func TestFailedCloneRetriesInsteadOfVMGone(t *testing.T) {
	e := newEnv(t)
	e.px.FailTask["clone"] = "clone failed: storage full"
	for i := 0; i < 8; i++ {
		_, _ = e.reconcile()
		if c := cond(e.get(), pxv1a1.ConditionInfrastructureReady); c != nil && c.Reason == pxv1a1.ReasonVMGone {
			t.Fatalf("failed clone reported as VMGone: %s", c.Message)
		}
	}
	if n := len(e.px.CallsWithPrefix("clone")); n != 1 {
		t.Fatalf("%d clones before the backoff expired, want 1", n)
	}
	e.clock = e.clock.Add(61 * time.Second)
	m := e.until("running", 20, isRunningWithAddress)
	if n := len(e.px.CallsWithPrefix("clone")); n != 2 || m.Status.VMID != firstID {
		t.Errorf("%d clones, vmid %d; want the retry into the same reserved %d", n, m.Status.VMID, firstID)
	}
}

// A console or backup task on the VM must not block a power change or
// mark the machine NotReady.
func TestForeignTasksDoNotBlock(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	e.px.AddTask("pve", firstID, "vncproxy")
	for i := 0; i < 3; i++ {
		_, _ = e.reconcile()
	}
	if c := cond(e.get(), pxv1a1.ConditionInfrastructureReady); c.Status != metav1.ConditionTrue {
		t.Fatalf("console made the machine NotReady: %s %s", c.Reason, c.Message)
	}
	e.setParent(func(vm *kubevmv1a1.VirtualMachine) { vm.Spec.PowerState = kubevmv1a1.PowerStateOff })
	e.until("stopped", 10, hasReason(pxv1a1.ConditionInfrastructureReady, pxv1a1.ReasonStopped))
}

// A task record that disappeared (task log rotated) must not wedge the
// machine in an error loop.
func TestVanishedTaskIsForgotten(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	pm := e.get()
	pm.Status.PendingTask = "UPID:pve:DEAD:0:0:qmstart:100:root@pam:"
	pm.Status.PendingOp = pxv1a1.OpStart
	if err := e.c.Status().Update(context.Background(), pm); err != nil {
		t.Fatal(err)
	}
	e.until("running again", 5, func(m *pxv1a1.ProxmoxMachine) bool {
		return m.Status.PendingTask == "" && isRunningWithAddress(m)
	})
}

// A VM paused by QEMU (I/O error) or a user reports "running" in
// status; it must not read as Ready.
func TestPausedVMIsNotReady(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	e.px.SetStatus(firstID, "paused")
	e.px.ListedStatus[firstID] = "running"
	m := e.until("Paused", 3, hasReason(pxv1a1.ConditionInfrastructureReady, pxv1a1.ReasonPaused))
	if m.Status.PowerState != "" {
		t.Errorf("powerState = %q for a paused VM, want absent", m.Status.PowerState)
	}
}

// An unreadable credentials Secret is an error (retried with backoff),
// not a silent park until the next resync.
func TestMissingCredentialsIsRetried(t *testing.T) {
	e := newEnv(t, parentVM(), machine())
	_, err := e.r.Reconcile(context.Background(), reqFor())
	if err == nil {
		t.Fatal("missing Secret returned no error, so it would not be retried")
	}
	if c := cond(e.get(), pxv1a1.ConditionInfrastructureReady); c.Reason != pxv1a1.ReasonInvalidConfiguration {
		t.Errorf("reason = %s", c.Reason)
	}
	if err := e.c.Create(context.Background(), secret()); err != nil {
		t.Fatal(err)
	}
	e.until("running once the Secret exists", 20, isRunningWithAddress)
}

// An LXC container squatting on the reserved ID answers every clone with
// "already exists", and is not a VM: the reservation must move on.
func TestContainerOnReservedIDIsSkipped(t *testing.T) {
	e := newEnv(t)
	e.px.TakeNextIDOnce = true
	e.px.TakeNextIDAsContainer = true
	m := e.until("running", 20, isRunningWithAddress)
	if m.Status.VMID != firstID+1 {
		t.Errorf("vmid = %d, want %d", m.Status.VMID, firstID+1)
	}
}

// Changing the power request clears the backoff from the old one.
func TestNewPowerRequestIgnoresOldBackoff(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	e.px.GuestRefusesShutdown = true
	e.setParent(func(vm *kubevmv1a1.VirtualMachine) {
		vm.Spec.PowerState = kubevmv1a1.PowerStateOff
		vm.Spec.PowerOffMode = kubevmv1a1.PowerOpModeSoft
	})
	e.until("Soft refused", 10, hasReason(pxv1a1.ConditionUpToDate, pxv1a1.ReasonTaskFailed))

	e.setParent(func(vm *kubevmv1a1.VirtualMachine) { vm.Spec.PowerOffMode = kubevmv1a1.PowerOpModeHard })
	e.until("stopped without waiting out the backoff", 10,
		hasReason(pxv1a1.ConditionInfrastructureReady, pxv1a1.ReasonStopped))
}

// A backoff from failed starts must not delay deleting the VM.
func TestDeleteIgnoresStartBackoff(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	e.crashLoop()
	e.until("start backoff", 10, func(m *pxv1a1.ProxmoxMachine) bool { return m.Status.NextAttempt != nil })
	e.deleteMachine()
	e.until("deleted without waiting out the backoff", 10, func(m *pxv1a1.ProxmoxMachine) bool { return m == nil })
	if strings.Join(e.px.CallsWithPrefix("delete"), "") != "delete 100" {
		t.Errorf("delete calls = %v", e.px.CallsWithPrefix("delete"))
	}
}

func reqFor() ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}
