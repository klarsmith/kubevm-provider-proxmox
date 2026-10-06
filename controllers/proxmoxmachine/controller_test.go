// SPDX-License-Identifier: Apache-2.0

package proxmoxmachine

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kubevmv1a1 "github.com/vmware-tanzu/vm-operator/external/kubevm/api/v1alpha1"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox/proxmoxfake"
)

const (
	ns      = "team-a"
	name    = "web-01"
	uid     = "11111111-2222-3333-4444-555555555555"
	pool    = pxv1a1.DefaultPool
	tmplID  = 9000
	firstID = 100
)

type env struct {
	t  *testing.T
	c  client.Client
	px *proxmoxfake.Fake
	r  *Reconciler

	// failStatusPatch, if set, may fail a status patch before it reaches
	// the API server (simulating a lost optimistic-lock race).
	failStatusPatch func(m *pxv1a1.ProxmoxMachine) error

	// clock is the reconciler's notion of now; tests advance it.
	clock time.Time
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme, pxv1a1.AddToScheme, kubevmv1a1.AddToScheme,
	} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func parentVM() *kubevmv1a1.VirtualMachine {
	mem := resource.MustParse("2Gi")
	return &kubevmv1a1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: kubevmv1a1.VirtualMachineSpec{
			InfrastructureRef: kubevmv1a1.ObjectReference{
				APIGroup: pxv1a1.GroupName, Kind: "ProxmoxMachine", Name: name,
			},
			PowerState:   kubevmv1a1.PowerStateOn,
			PowerOffMode: kubevmv1a1.PowerOpModeTrySoft,
			InstanceType: &kubevmv1a1.InstanceTypeSpec{
				Resources: &kubevmv1a1.ResourceSpec{CPUs: 2, Memory: &mem},
			},
			BootDisk: &kubevmv1a1.BootDiskSpec{
				Source: kubevmv1a1.DiskSource{Image: &kubevmv1a1.ObjectReference{
					APIGroup: pxv1a1.GroupName, Kind: "ProxmoxTemplate", Name: "debian-12",
				}},
				SizeGiB: ptr.To[int64](10),
			},
			SSHPublicKeys: []string{"ssh-ed25519 AAAAC3Nza test@example"},
		},
	}
}

func machine() *pxv1a1.ProxmoxMachine {
	return &pxv1a1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, UID: types.UID(uid),
			Annotations: map[string]string{pxv1a1.AnnotationKey: name},
		},
		Spec: pxv1a1.ProxmoxMachineSpec{
			CredentialsSecretRef: corev1.LocalObjectReference{Name: "pve"},
			Pool:                 pool,
			Bridge:               "vmbr0",
			FullClone:            ptr.To(true),
		},
	}
}

func secret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pve"},
		Data: map[string][]byte{
			"url": []byte("https://pve.test:8006"), "tokenID": []byte("kubevm@pve!ctl"),
			"tokenSecret": []byte("s3cret"),
		},
	}
}

func newEnv(t *testing.T, objs ...client.Object) *env {
	t.Helper()
	if objs == nil {
		objs = []client.Object{parentVM(), machine(), secret()}
	}
	e := &env{t: t}
	c := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(objs...).
		WithStatusSubresource(&pxv1a1.ProxmoxMachine{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, sub string, obj client.Object,
				patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if m, ok := obj.(*pxv1a1.ProxmoxMachine); ok && e.failStatusPatch != nil {
					if err := e.failStatusPatch(m); err != nil {
						return err
					}
				}
				return c.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).Build()
	px := proxmoxfake.New()
	r := &Reconciler{
		Client:      c,
		AllowedPool: pool,
		NewProxmoxClient: func(creds proxmox.Credentials) (proxmox.Client, error) {
			if creds.TokenID != "kubevm@pve!ctl" {
				t.Errorf("credentials not read from the Secret: %+v", creds)
			}
			return px, nil
		},
	}
	e.clock = time.Now()
	r.Now = func() time.Time { return e.clock }
	e.c, e.px, e.r = c, px, r
	return e
}

func (e *env) reconcile() (ctrl.Result, error) {
	return e.r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
}

// get returns the current ProxmoxMachine, or nil once it is gone.
func (e *env) get() *pxv1a1.ProxmoxMachine {
	e.t.Helper()
	m := &pxv1a1.ProxmoxMachine{}
	err := e.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, m)
	if client.IgnoreNotFound(err) != nil {
		e.t.Fatal(err)
	}
	if err != nil {
		return nil
	}
	return m
}

// until reconciles until cond holds, failing after max passes. Reconcile
// errors are tolerated: the real controller retries them too.
func (e *env) until(what string, max int, cond func(*pxv1a1.ProxmoxMachine) bool) *pxv1a1.ProxmoxMachine {
	e.t.Helper()
	var lastErr error
	for i := 0; i < max; i++ {
		_, lastErr = e.reconcile()
		if m := e.get(); cond(m) {
			return m
		}
	}
	m := e.get()
	e.t.Fatalf("not %s after %d reconciles (last error %v); status: %+v", what, max, lastErr, statusOf(m))
	return nil
}

func statusOf(m *pxv1a1.ProxmoxMachine) any {
	if m == nil {
		return "<deleted>"
	}
	return m.Status
}

func cond(m *pxv1a1.ProxmoxMachine, t string) *metav1.Condition {
	if m == nil {
		return nil
	}
	return meta.FindStatusCondition(m.Status.Conditions, t)
}

func hasReason(t, reason string) func(*pxv1a1.ProxmoxMachine) bool {
	return func(m *pxv1a1.ProxmoxMachine) bool {
		c := cond(m, t)
		return c != nil && c.Reason == reason
	}
}

func isRunningWithAddress(m *pxv1a1.ProxmoxMachine) bool {
	c := cond(m, pxv1a1.ConditionInfrastructureReady)
	return c != nil && c.Status == metav1.ConditionTrue && c.Reason == pxv1a1.ReasonRunning &&
		len(m.Status.Addresses) > 0
}

func (e *env) setParent(mut func(*kubevmv1a1.VirtualMachine)) {
	e.t.Helper()
	vm := &kubevmv1a1.VirtualMachine{}
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, vm); err != nil {
		e.t.Fatal(err)
	}
	mut(vm)
	if err := e.c.Update(context.Background(), vm); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) deleteMachine() {
	e.t.Helper()
	if err := e.c.Delete(context.Background(), e.get()); err != nil {
		e.t.Fatal(err)
	}
}

func TestCreateProvisionsAndStarts(t *testing.T) {
	e := newEnv(t)
	m := e.until("running with an address", 20, isRunningWithAddress)

	if m.Status.VMID != firstID {
		t.Fatalf("vmid = %d, want %d", m.Status.VMID, firstID)
	}
	if got, want := m.Status.ProviderID, "proxmox://testcluster/100"; got != want {
		t.Errorf("providerID = %q, want %q", got, want)
	}
	if m.Status.PowerState != "PoweredOn" {
		t.Errorf("powerState = %q", m.Status.PowerState)
	}
	if got := m.Status.Addresses[0]; got.Address != "10.0.0.100" || got.Type != pxv1a1.AddressInternalIP {
		t.Errorf("address = %+v", got)
	}
	if len(m.Status.Addresses) != 1 {
		t.Errorf("loopback/link-local not filtered: %+v", m.Status.Addresses)
	}
	if m.Spec.Template != "debian-12" || m.Spec.CPUs != 2 || m.Spec.MemoryMiB != 2048 {
		t.Errorf("spec not resolved from parent: %+v", m.Spec)
	}
	if c := cond(m, pxv1a1.ConditionUpToDate); c == nil || c.Status != metav1.ConditionTrue {
		t.Errorf("UpToDate = %+v", c)
	}

	vm, ok := e.px.Get(firstID)
	if !ok {
		t.Fatal("VM 100 not created")
	}
	if vm.Pool != pool {
		t.Errorf("VM pool = %q", vm.Pool)
	}
	if !strings.Contains(vm.Config["description"], "kubevm-uid="+uid) {
		t.Errorf("identity marker missing: %q", vm.Config["description"])
	}
	if vm.Config["cores"] != "2" || vm.Config["memory"] != "2048" {
		t.Errorf("sizing not applied: cores=%s memory=%s", vm.Config["cores"], vm.Config["memory"])
	}
	if !strings.Contains(vm.Config["scsi0"], "size=10G") {
		t.Errorf("boot disk not grown: %s", vm.Config["scsi0"])
	}
	if !strings.Contains(vm.Config["sshkeys"], "ssh-ed25519%20AAAAC3Nza") {
		t.Errorf("sshkeys not encoded: %q", vm.Config["sshkeys"])
	}

	// Pre-boot configuration must land before the first start.
	order := strings.Join(e.px.Calls, " | ")
	if strings.Index(order, "resize") > strings.Index(order, "start") ||
		strings.Index(order, "config") > strings.Index(order, "start") {
		t.Errorf("start issued before provisioning: %s", order)
	}
	if n := len(e.px.CallsWithPrefix("clone")); n != 1 {
		t.Errorf("%d clones, want 1", n)
	}
	if got := e.parentAnnotation(); got != contractHash(m) {
		t.Errorf("parent not nudged with the published status: annotation %q, want %q", got, contractHash(m))
	}
}

func (e *env) parentAnnotation() string {
	e.t.Helper()
	vm := &kubevmv1a1.VirtualMachine{}
	if err := e.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, vm); err != nil {
		e.t.Fatal(err)
	}
	return vm.Annotations[StatusHashAnnotation]
}

func TestAdoptsVMWhoseIDWasNeverRecorded(t *testing.T) {
	e := newEnv(t)
	// A previous run cloned VM 150 and crashed before writing status.vmid.
	e.px.AddVM(proxmoxfake.FakeVM{
		VM:     proxmox.VM{VMID: 150, Node: "pve", Name: name, Pool: pool, Status: "stopped"},
		Config: map[string]string{"description": "kubevm-uid=" + uid + "\nManaged by ...", "scsi0": "local-lvm:vm-150-disk-0,size=4G", "boot": "order=scsi0"},
	})
	m := e.until("running", 20, func(m *pxv1a1.ProxmoxMachine) bool {
		return m.Status.PowerState == "PoweredOn"
	})
	if m.Status.VMID != 150 {
		t.Errorf("vmid = %d, want the adopted 150", m.Status.VMID)
	}
	if calls := e.px.CallsWithPrefix("clone"); len(calls) != 0 {
		t.Errorf("cloned again instead of adopting: %v", calls)
	}
}

func TestMarkerOutsidePoolIsNotAdopted(t *testing.T) {
	e := newEnv(t)
	e.px.AddVM(proxmoxfake.FakeVM{
		VM:     proxmox.VM{VMID: 150, Node: "pve", Name: name, Pool: "production", Status: "running"},
		Config: map[string]string{"description": "kubevm-uid=" + uid},
	})
	m := e.until("running", 20, isRunningWithAddress)
	if m.Status.VMID == 150 {
		t.Fatal("adopted a VM outside the allowed pool")
	}
	if vm, _ := e.px.Get(150); vm.Status != "running" {
		t.Errorf("VM outside the pool was touched: %+v", vm.VM)
	}
}

func TestVMIDRaceRetriesWithFreshID(t *testing.T) {
	e := newEnv(t)
	e.px.TakeNextIDOnce = true
	m := e.until("running", 20, isRunningWithAddress)
	if m.Status.VMID != firstID+1 {
		t.Errorf("vmid = %d, want %d after losing %d", m.Status.VMID, firstID+1, firstID)
	}
	if n := len(e.px.CallsWithPrefix("clone")); n != 1 {
		t.Errorf("%d successful clones, want 1", n)
	}
	if vm, _ := e.px.Get(firstID); vm.Name != "someone-else" {
		t.Errorf("the other client's VM was changed: %+v", vm)
	}
}

func TestCloneTaskFailureIsReported(t *testing.T) {
	e := newEnv(t)
	e.px.FailTask["clone"] = "can't allocate space in storage local-lvm"
	m := e.until("TaskFailed", 10, hasReason(pxv1a1.ConditionInfrastructureReady, pxv1a1.ReasonTaskFailed))
	if c := cond(m, pxv1a1.ConditionInfrastructureReady); !strings.Contains(c.Message, "can't allocate") {
		t.Errorf("exit status not surfaced: %q", c.Message)
	}
	if m.Status.PendingTask != "" {
		t.Error("failed task left pending")
	}
}

func TestPowerOff(t *testing.T) {
	for _, tc := range []struct {
		mode     kubevmv1a1.PowerOpMode
		wantCall string
	}{
		{kubevmv1a1.PowerOpModeHard, "stop 100"},
		{kubevmv1a1.PowerOpModeSoft, "shutdown 100"},
		{kubevmv1a1.PowerOpModeTrySoft, "shutdown 100"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			e := newEnv(t)
			e.until("running", 20, isRunningWithAddress)
			e.setParent(func(vm *kubevmv1a1.VirtualMachine) {
				vm.Spec.PowerState = kubevmv1a1.PowerStateOff
				vm.Spec.PowerOffMode = tc.mode
			})
			m := e.until("stopped", 10, hasReason(pxv1a1.ConditionInfrastructureReady, pxv1a1.ReasonStopped))
			if m.Status.PowerState != "PoweredOff" || len(m.Status.Addresses) != 0 {
				t.Errorf("status = %+v", m.Status)
			}
			if calls := e.px.CallsWithPrefix(strings.Fields(tc.wantCall)[0]); len(calls) != 1 || calls[0] != tc.wantCall {
				t.Errorf("calls = %v, want [%s]", calls, tc.wantCall)
			}

			e.setParent(func(vm *kubevmv1a1.VirtualMachine) { vm.Spec.PowerState = kubevmv1a1.PowerStateOn })
			e.until("running again", 10, isRunningWithAddress)
		})
	}
}

func TestSoftPowerOffFailsWhenGuestRefuses(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	e.px.GuestRefusesShutdown = true
	e.setParent(func(vm *kubevmv1a1.VirtualMachine) {
		vm.Spec.PowerState = kubevmv1a1.PowerStateOff
		vm.Spec.PowerOffMode = kubevmv1a1.PowerOpModeSoft
	})
	m := e.until("UpToDate=TaskFailed", 10, hasReason(pxv1a1.ConditionUpToDate, pxv1a1.ReasonTaskFailed))
	if e.px.CallsWithPrefix("stop") != nil {
		t.Error("Soft escalated to a hard stop")
	}
	if vm, _ := e.px.Get(firstID); vm.Status != "running" {
		t.Errorf("VM status = %s, want still running", vm.Status)
	}
	_ = m
}

func TestVMRemovedOutOfBand(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	e.px.Remove(firstID)
	m := e.until("VMGone", 3, hasReason(pxv1a1.ConditionInfrastructureReady, pxv1a1.ReasonVMGone))
	if m.Status.PowerState != "" || len(m.Status.Addresses) != 0 {
		t.Errorf("stale observations kept: %+v", m.Status)
	}
	for i := 0; i < 3; i++ {
		_, _ = e.reconcile()
	}
	if n := len(e.px.CallsWithPrefix("clone")); n != 1 {
		t.Errorf("silently recreated the VM (%d clones)", n)
	}
}

func TestDeleteStopsThenDestroys(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	e.deleteMachine()
	e.until("deleted", 10, func(m *pxv1a1.ProxmoxMachine) bool { return m == nil })
	if _, ok := e.px.Get(firstID); ok {
		t.Error("VM still exists")
	}
	if got := strings.Join(e.px.CallsWithPrefix("stop"), ""); got != "stop 100" {
		t.Errorf("not hard-stopped before destroy: %q", got)
	}
	if got := e.px.CallsWithPrefix("delete"); len(got) != 1 {
		t.Errorf("delete calls = %v", got)
	}
}

func TestDeleteWithVMAlreadyGone(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	e.px.Remove(firstID)
	e.deleteMachine()
	e.until("deleted", 5, func(m *pxv1a1.ProxmoxMachine) bool { return m == nil })
	if got := e.px.CallsWithPrefix("delete"); got != nil {
		t.Errorf("delete issued for a missing VM: %v", got)
	}
}

func TestDeleteMidClone(t *testing.T) {
	e := newEnv(t)
	e.px.TaskPolls = 3
	e.until("cloning", 5, func(m *pxv1a1.ProxmoxMachine) bool { return m.Status.PendingOp == pxv1a1.OpClone })
	e.deleteMachine()
	e.until("deleted", 20, func(m *pxv1a1.ProxmoxMachine) bool { return m == nil })
	if _, ok := e.px.Get(firstID); ok {
		t.Error("clone leaked")
	}
}

func TestSpecPoolOutsideAllowedPoolIsRefused(t *testing.T) {
	m := machine()
	m.Spec.Pool = "production"
	e := newEnv(t, parentVM(), m, secret())
	e.until("OutsidePool", 3, hasReason(pxv1a1.ConditionInfrastructureReady, pxv1a1.ReasonOutsidePool))
	if len(e.px.Calls) != 0 {
		t.Errorf("Proxmox was called: %v", e.px.Calls)
	}
}

func TestVMMovedOutOfPoolIsLeftAlone(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	e.px.VMs[firstID].Pool = "production"
	before := len(e.px.Calls)

	e.setParent(func(vm *kubevmv1a1.VirtualMachine) { vm.Spec.PowerState = kubevmv1a1.PowerStateOff })
	e.until("OutsidePool", 3, hasReason(pxv1a1.ConditionInfrastructureReady, pxv1a1.ReasonOutsidePool))

	e.deleteMachine()
	e.until("deleted", 5, func(m *pxv1a1.ProxmoxMachine) bool { return m == nil })
	if after := e.px.Calls[before:]; len(after) != 0 {
		t.Errorf("acted on a VM outside the pool: %v", after)
	}
	if vm, ok := e.px.Get(firstID); !ok || vm.Status != "running" {
		t.Error("VM outside the pool was changed or deleted")
	}
}

func TestNotAdoptedWithoutBackReference(t *testing.T) {
	m := machine()
	m.Annotations = nil
	e := newEnv(t, parentVM(), m, secret())
	e.until("NotAdopted", 2, hasReason(pxv1a1.ConditionInfrastructureReady, pxv1a1.ReasonNotAdopted))
	if len(e.px.Calls) != 0 {
		t.Errorf("Proxmox was called: %v", e.px.Calls)
	}
}

func TestUnsupportedFieldsAreReported(t *testing.T) {
	e := newEnv(t)
	e.setParent(func(vm *kubevmv1a1.VirtualMachine) {
		vm.Spec.InstanceType.Name = "large"
		vm.Spec.Disks = []kubevmv1a1.DiskSpec{{Name: "data", Source: kubevmv1a1.DiskSource{Blank: true}}}
	})
	m := e.until("UnsupportedByProvider", 20,
		hasReason(pxv1a1.ConditionUpToDate, pxv1a1.ReasonUnsupportedByProvider))
	c := cond(m, pxv1a1.ConditionUpToDate)
	if !strings.Contains(c.Message, "instanceType.name") || !strings.Contains(c.Message, "data disks") {
		t.Errorf("message = %q", c.Message)
	}
}

func TestStaticAddressNeedsPrefixLength(t *testing.T) {
	e := newEnv(t)
	e.setParent(func(vm *kubevmv1a1.VirtualMachine) {
		vm.Spec.Network = &kubevmv1a1.NetworkSpec{Interfaces: []kubevmv1a1.NetworkInterfaceSpec{
			{Name: "primary", Addresses: []string{"192.168.10.20"}},
		}}
	})
	e.until("InvalidConfiguration", 3,
		hasReason(pxv1a1.ConditionInfrastructureReady, pxv1a1.ReasonInvalidConfiguration))
	if len(e.px.Calls) != 0 {
		t.Errorf("Proxmox was called: %v", e.px.Calls)
	}
}

func TestStaticAddressAndPortableInterfaceName(t *testing.T) {
	m := machine()
	m.Spec.StaticIPPrefixLength = 24
	m.Spec.Gateway = "192.168.10.1"
	e := newEnv(t, parentVM(), m, secret())
	e.setParent(func(vm *kubevmv1a1.VirtualMachine) {
		vm.Spec.Network = &kubevmv1a1.NetworkSpec{
			Nameservers: []string{"1.1.1.1", "9.9.9.9"},
			Interfaces: []kubevmv1a1.NetworkInterfaceSpec{
				{Name: "primary", Addresses: []string{"192.168.10.20"}},
			},
		}
	})
	got := e.until("running", 20, isRunningWithAddress)
	vm, _ := e.px.Get(firstID)
	if vm.Config["ipconfig0"] != "ip=192.168.10.20/24,gw=192.168.10.1" {
		t.Errorf("ipconfig0 = %q", vm.Config["ipconfig0"])
	}
	if vm.Config["nameserver"] != "1.1.1.1 9.9.9.9" {
		t.Errorf("nameserver = %q", vm.Config["nameserver"])
	}
	if vm.Config["net0"] != "virtio=BC:24:11:00:00:01,bridge=vmbr0" {
		t.Errorf("net0 MAC not kept: %q", vm.Config["net0"])
	}
	if got.Status.Addresses[0].Interface != "primary" {
		t.Errorf("address interface = %q, want the portable name", got.Status.Addresses[0].Interface)
	}
}

// A power call succeeds but the status write recording its task is lost.
// The retry must find the running task in Proxmox, not send a second call.
func TestLostStatusWriteDoesNotRepeatPowerOp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *env)
		op    string
		call  string
	}{
		// Found on real PVE 9.2: the lost write was the clone's, and the VM
		// mid-clone is not yet in its pool, so the marker lookup missed it.
		{"clone", func(e *env) {}, pxv1a1.OpClone, "clone"},
		{"start", func(e *env) {}, pxv1a1.OpStart, "start"},
		{"shutdown", func(e *env) {
			e.until("running", 20, isRunningWithAddress)
			e.setParent(func(vm *kubevmv1a1.VirtualMachine) { vm.Spec.PowerState = kubevmv1a1.PowerStateOff })
		}, pxv1a1.OpShutdown, "shutdown"},
		{"delete", func(e *env) {
			e.until("running", 20, isRunningWithAddress)
			e.deleteMachine()
		}, pxv1a1.OpStop, "stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.px.TaskPolls = 2
			tc.setup(e)

			lost := false
			e.failStatusPatch = func(m *pxv1a1.ProxmoxMachine) error {
				if !lost && m.Status.PendingOp == tc.op {
					lost = true
					return apierrors.NewServiceUnavailable("etcdserver: request timed out")
				}
				return nil
			}
			for i := 0; i < 15; i++ {
				_, _ = e.reconcile()
			}
			if !lost {
				t.Fatalf("no %s status write was intercepted", tc.op)
			}
			if got := e.px.CallsWithPrefix(tc.call); len(got) != 1 {
				t.Errorf("%s calls = %v, want exactly one", tc.call, got)
			}
		})
	}
}

func nonTemplateVMs(f *proxmoxfake.Fake) []int {
	vms, _ := f.Resources(context.Background())
	var out []int
	for _, vm := range vms {
		if !vm.Template {
			out = append(out, vm.VMID)
		}
	}
	return out
}

func TestDeleteSweepsDuplicateMarkerVMs(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	// A duplicate left by an earlier bug: same marker, in the pool.
	e.px.AddVM(proxmoxfake.FakeVM{
		VM:     proxmox.VM{VMID: 150, Node: "pve", Name: name, Pool: pool, Status: "running"},
		Config: map[string]string{"description": "kubevm-uid=" + uid},
	})
	// And someone else's VM in the pool, which must survive.
	e.px.AddVM(proxmoxfake.FakeVM{
		VM:     proxmox.VM{VMID: 160, Node: "pve", Name: "other", Pool: pool, Status: "running"},
		Config: map[string]string{"description": "kubevm-uid=99999999-0000-0000-0000-000000000000"},
	})
	e.deleteMachine()
	e.until("deleted", 20, func(m *pxv1a1.ProxmoxMachine) bool { return m == nil })
	if got := nonTemplateVMs(e.px); len(got) != 1 || got[0] != 160 {
		t.Errorf("VMs left = %v, want only the unrelated 160", got)
	}
}

func TestDeleteWhileUnrecordedCloneIsInFlight(t *testing.T) {
	e := newEnv(t)
	e.px.TaskPolls = 4
	lost := false
	e.failStatusPatch = func(m *pxv1a1.ProxmoxMachine) error {
		if !lost && m.Status.PendingOp == pxv1a1.OpClone && m.Status.PendingTask != "" {
			lost = true
			return apierrors.NewServiceUnavailable("etcdserver: request timed out")
		}
		return nil
	}
	for i := 0; i < 2; i++ {
		_, _ = e.reconcile()
	}
	if !lost {
		t.Fatal("the clone's status write was not intercepted")
	}
	e.deleteMachine()
	e.until("deleted", 30, func(m *pxv1a1.ProxmoxMachine) bool { return m == nil })
	if got := nonTemplateVMs(e.px); len(got) != 0 {
		t.Errorf("VMs leaked: %v", got)
	}
	if n := len(e.px.CallsWithPrefix("clone")); n != 1 {
		t.Errorf("%d clones, want 1", n)
	}
}

// crashLoop makes VM 100 crash and every later start fail, while the
// resource listing still says "running" (its cache lags), as seen on a real
// PVE whose guests could not start.
func (e *env) crashLoop() {
	e.px.SetStatus(firstID, "stopped")
	e.px.ListedStatus[firstID] = "running"
	e.px.AlwaysFailTask["start"] = "start failed: QEMU exited with code 1"
}

func TestStaleListingNeverReportsACrashedVMReady(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	e.crashLoop()

	for i := 0; i < 15; i++ {
		_, _ = e.reconcile()
		m := e.get()
		if c := cond(m, pxv1a1.ConditionInfrastructureReady); c != nil && c.Status == metav1.ConditionTrue {
			t.Fatalf("reconcile %d: crashed VM reported Ready (%s: %s)", i, c.Reason, c.Message)
		}
	}
	m := e.get()
	if c := cond(m, pxv1a1.ConditionInfrastructureReady); c.Reason != pxv1a1.ReasonTaskFailed ||
		!strings.Contains(c.Message, "QEMU exited with code 1") {
		t.Errorf("InfrastructureReady = %s: %s", c.Reason, c.Message)
	}
	if c := cond(m, pxv1a1.ConditionUpToDate); c.Status != metav1.ConditionFalse {
		t.Errorf("UpToDate = %s %s, want False while the VM cannot start", c.Status, c.Reason)
	}
}

func TestFailedStartsBackOffAndRecover(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	e.crashLoop()
	starts := func() int { return len(e.px.CallsWithPrefix("start")) }
	settle := func() {
		for i := 0; i < 10; i++ {
			_, _ = e.reconcile()
		}
	}

	settle() // first retry fails; no clock advance means no further attempt
	if got := starts(); got != 2 {
		t.Fatalf("starts = %d, want 2 (initial + one retry) before the backoff expires", got)
	}
	if m := e.get(); m.Status.ConsecutiveFailures != 1 || m.Status.NextAttempt == nil {
		t.Fatalf("failure not recorded: %+v", m.Status)
	}

	e.clock = e.clock.Add(61 * time.Second) // past the 1m backoff
	settle()
	if got := starts(); got != 3 {
		t.Fatalf("starts = %d after 1m, want 3", got)
	}

	e.clock = e.clock.Add(61 * time.Second) // second backoff is 2m: not yet
	settle()
	if got := starts(); got != 3 {
		t.Fatalf("starts = %d 1m into a 2m backoff, want still 3", got)
	}

	delete(e.px.AlwaysFailTask, "start") // the host is fixed
	e.clock = e.clock.Add(2 * time.Minute)
	m := e.until("running again", 20, isRunningWithAddress)
	if m.Status.ConsecutiveFailures != 0 || m.Status.NextAttempt != nil || m.Status.LastFailure != "" {
		t.Errorf("failure streak not reset: %+v", m.Status)
	}
	if c := cond(m, pxv1a1.ConditionUpToDate); c.Status != metav1.ConditionTrue {
		t.Errorf("UpToDate = %s %s after recovery", c.Status, c.Reason)
	}
}

func TestDeleteWithStaleListingStopsOnce(t *testing.T) {
	e := newEnv(t)
	e.until("running", 20, isRunningWithAddress)
	// The listing keeps saying "running" long after the stop lands.
	e.px.ListedStatus[firstID] = "running"
	e.deleteMachine()
	e.until("deleted", 20, func(m *pxv1a1.ProxmoxMachine) bool { return m == nil })
	if got := e.px.CallsWithPrefix("stop"); len(got) != 1 {
		t.Errorf("stop calls = %v, want exactly one", got)
	}
}
