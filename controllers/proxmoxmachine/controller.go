// SPDX-License-Identifier: Apache-2.0

// Package proxmoxmachine reconciles a ProxmoxMachine into a Proxmox VE VM.
//
// Split like the AWS provider the KubeVM guide describes (controller /
// create / observe / power / addresses / delete) because Proxmox has the
// same kinds of problem: every state change is an asynchronous task, VM
// names are not unique, and /cluster/nextid can race.
package proxmoxmachine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kubevmv1a1 "github.com/vmware-tanzu/vm-operator/external/kubevm/api/v1alpha1"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/link"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
)

const (
	// pollRequeueDelay matches the KubeVM core's own delay for transitional
	// states.
	pollRequeueDelay = 10 * time.Second

	// failureRequeueDelay is the wait after a failed Proxmox task before
	// trying again.
	failureRequeueDelay = time.Minute

	// resyncPeriod re-observes a settled VM. Proxmox VMs change out-of-band
	// (HA migration, someone in the web UI), and nothing notifies us.
	resyncPeriod = 2 * time.Minute
)

// ClientFactory builds a Proxmox client from credentials. Tests return a
// proxmoxfake.Fake.
type ClientFactory func(creds proxmox.Credentials) (proxmox.Client, error)

// Reconciler turns a ProxmoxMachine into a Proxmox VM.
type Reconciler struct {
	client.Client

	// AllowedPool is the only Proxmox pool this controller creates, changes
	// or deletes VMs in. Anything outside it is refused.
	AllowedPool string

	// NewProxmoxClient builds the Proxmox client. Nil means the real HTTP
	// client.
	NewProxmoxClient ClientFactory

	// PollInterval overrides pollRequeueDelay (tests).
	PollInterval time.Duration

	// Now overrides the clock (tests). Nil means time.Now.
	Now func() time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// maxBackoff caps the wait between attempts after repeated task failures.
const maxBackoff = 30 * time.Minute

// backoffFor is failureRequeueDelay doubled per consecutive failure, capped.
func backoffFor(failures int32) time.Duration {
	if failures < 1 {
		failures = 1
	}
	if failures > 10 {
		return maxBackoff
	}
	return min(failureRequeueDelay<<(failures-1), maxBackoff)
}

// recordTaskFailure counts a failed task and sets when the next task may
// start. The gate lives in status, not in a requeue delay, because any
// event (a status write, a VirtualMachine edit) triggers a reconcile
// immediately and would otherwise retry at once.
func (r *Reconciler) recordTaskFailure(m *pxv1a1.ProxmoxMachine, msg string) {
	m.Status.ConsecutiveFailures++
	m.Status.LastFailure = msg
	next := metav1.NewTime(r.now().Add(backoffFor(m.Status.ConsecutiveFailures)).Truncate(time.Second))
	m.Status.NextAttempt = &next
}

// recordTaskSuccess clears the failure streak.
func recordTaskSuccess(m *pxv1a1.ProxmoxMachine) {
	m.Status.ConsecutiveFailures = 0
	m.Status.LastFailure = ""
	m.Status.NextAttempt = nil
}

// waitRemaining is how long until another task may start; 0 means now.
func (r *Reconciler) waitRemaining(m *pxv1a1.ProxmoxMachine) time.Duration {
	if m.Status.NextAttempt == nil {
		return 0
	}
	return max(m.Status.NextAttempt.Sub(r.now()), 0)
}

// failureMessage describes the failure streak for conditions.
func (r *Reconciler) failureMessage(m *pxv1a1.ProxmoxMachine) string {
	msg := fmt.Sprintf("%s (failed %d time(s) in a row)", m.Status.LastFailure, m.Status.ConsecutiveFailures)
	if m.Status.NextAttempt != nil {
		msg += "; next attempt at " + m.Status.NextAttempt.UTC().Format(time.RFC3339)
	}
	return msg
}

func (r *Reconciler) pollDelay() time.Duration {
	if r.PollInterval > 0 {
		return r.PollInterval
	}
	return pollRequeueDelay
}

// +kubebuilder:rbac:groups=infrastructure.kube-vm.io,resources=proxmoxmachines,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=infrastructure.kube-vm.io,resources=proxmoxmachines/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=kube-vm.io,resources=virtualmachines,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile drives one ProxmoxMachine towards what its parent asks for.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	m := &pxv1a1.ProxmoxMachine{}
	if err := r.Get(ctx, req.NamespacedName, m); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !m.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, m)
	}
	return r.reconcileNormal(ctx, m)
}

func (r *Reconciler) reconcileNormal(ctx context.Context, m *pxv1a1.ProxmoxMachine) (ctrl.Result, error) {
	// No Proxmox call before the link is confirmed mutual.
	named := m.GetAnnotations()[pxv1a1.AnnotationKey]
	parent, err := link.ParentOf(ctx, r.Client, m)
	switch {
	case errors.Is(err, link.ErrNotLinked):
		return ctrl.Result{}, r.markNotReady(ctx, m, pxv1a1.ReasonNotAdopted,
			fmt.Sprintf("no %s annotation: add one naming the VirtualMachine this object belongs to",
				pxv1a1.AnnotationKey))
	case errors.Is(err, link.ErrParentMissing):
		return ctrl.Result{RequeueAfter: r.pollDelay()}, r.markNotReady(ctx, m, pxv1a1.ReasonNotAdopted,
			fmt.Sprintf("VirtualMachine %q, named by the %s annotation, does not exist yet",
				named, pxv1a1.AnnotationKey))
	case errors.Is(err, link.ErrNotMutual):
		return ctrl.Result{}, r.markNotReady(ctx, m, pxv1a1.ReasonNotAdopted,
			fmt.Sprintf("VirtualMachine %q does not name this ProxmoxMachine in spec.infrastructureRef; "+
				"a link needs both sides", named))
	case err != nil:
		return ctrl.Result{}, err
	}

	if err := r.claim(ctx, m); err != nil {
		return ctrl.Result{}, err
	}
	prevPower, prevMode := m.Spec.PowerState, m.Spec.PowerOffMode
	if err := r.persistResolved(ctx, m, parent); err != nil {
		if apierrors.IsInvalid(err) {
			return ctrl.Result{}, r.markNotReady(ctx, m, pxv1a1.ReasonInvalidConfiguration, err.Error())
		}
		return ctrl.Result{}, err
	}

	if reason, msg := r.validate(m); reason != "" {
		return ctrl.Result{}, r.markNotReady(ctx, m, reason, msg)
	}

	px, err := r.proxmoxClient(ctx, m)
	if err != nil {
		return ctrl.Result{}, r.credentialsError(ctx, m, "", err)
	}

	sw := r.newStatusWriter(m)
	if m.Spec.PowerState != prevPower || m.Spec.PowerOffMode != prevMode {
		// A new request is not bound by the old one's failures: flipping a
		// refused Soft power-off to Hard should act now, not in 30 minutes.
		recordTaskSuccess(m)
	}
	res, cerr := r.converge(ctx, m, px, unsupported(parent), sw)
	if cerr != nil {
		setCondition(m, notReady(pxv1a1.ReasonAPIError, cerr.Error()))
	}
	if err := sw.flush(ctx, m); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.nudgeParent(ctx, m, parent); err != nil {
		return ctrl.Result{}, err
	}
	return res, cerr
}

// validate checks what can be checked without calling Proxmox.
func (r *Reconciler) validate(m *pxv1a1.ProxmoxMachine) (reason, message string) {
	if m.Spec.Pool != r.AllowedPool {
		return pxv1a1.ReasonOutsidePool, fmt.Sprintf(
			"spec.pool is %q, but this controller only acts in pool %q", m.Spec.Pool, r.AllowedPool)
	}
	if m.Status.VMID == 0 && m.Spec.Template == "" {
		return pxv1a1.ReasonInvalidConfiguration,
			"no template named in the VirtualMachine's spec.bootDisk.source.image.name"
	}
	if m.Spec.CredentialsSecretRef.Name == "" {
		return pxv1a1.ReasonInvalidConfiguration, "spec.credentialsSecretRef.name is empty"
	}
	for _, ifc := range m.Spec.Interfaces {
		if len(ifc.Addresses) > 0 && m.Spec.StaticIPPrefixLength == 0 {
			// KubeVM addresses carry no prefix; Proxmox ipconfigN needs one.
			return pxv1a1.ReasonInvalidConfiguration, fmt.Sprintf(
				"interface %q has static addresses, but spec.staticIPPrefixLength is not set", ifc.Name)
		}
	}
	return "", ""
}

// proxmoxClient reads the credentials Secret and builds a client.
func (r *Reconciler) proxmoxClient(ctx context.Context, m *pxv1a1.ProxmoxMachine) (proxmox.Client, error) {
	s := &corev1.Secret{}
	key := client.ObjectKey{Namespace: m.Namespace, Name: m.Spec.CredentialsSecretRef.Name}
	if err := r.Get(ctx, key, s); err != nil {
		return nil, fmt.Errorf("reading credentials Secret %q: %w", key.Name, err)
	}
	creds := proxmox.Credentials{
		URL:                string(s.Data["url"]),
		TokenID:            string(s.Data["tokenID"]),
		TokenSecret:        string(s.Data["tokenSecret"]),
		CABundle:           s.Data["caBundle"],
		InsecureSkipVerify: strings.EqualFold(string(s.Data["insecureSkipVerify"]), "true"),
	}
	factory := r.NewProxmoxClient
	if factory == nil {
		factory = func(c proxmox.Credentials) (proxmox.Client, error) { return proxmox.NewHTTPClient(c) }
	}
	px, err := factory(creds)
	if err != nil {
		return nil, fmt.Errorf("credentials Secret %q: %w", key.Name, err)
	}
	return px, nil
}

// credentialsError reports an unusable credentials Secret and returns the
// error, so controller-runtime retries with its own exponential backoff.
// Secrets are not watched; without the retry a Secret created or fixed
// later would go unnoticed until the next resync.
func (r *Reconciler) credentialsError(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	prefix string, err error) error {

	if merr := r.markNotReady(ctx, m, pxv1a1.ReasonInvalidConfiguration, prefix+err.Error()); merr != nil {
		return merr
	}
	return err
}

// claim adds the finalizer before any Proxmox call, so a VM is never
// created for an object that could be deleted without cleanup.
func (r *Reconciler) claim(ctx context.Context, m *pxv1a1.ProxmoxMachine) error {
	base := m.DeepCopy()
	controllerutil.AddFinalizer(m, pxv1a1.Finalizer)
	return r.patchIfChanged(ctx, m, base)
}

func (r *Reconciler) patchIfChanged(ctx context.Context, m, base *pxv1a1.ProxmoxMachine) error {
	if equality.Semantic.DeepEqual(base, m) {
		return nil
	}
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := r.Patch(ctx, m, patch); err != nil {
		return fmt.Errorf("patching %s/%s: %w", m.Namespace, m.Name, err)
	}
	return nil
}

func (r *Reconciler) patchStatusIfChanged(ctx context.Context, m, base *pxv1a1.ProxmoxMachine) error {
	if equality.Semantic.DeepEqual(base.Status, m.Status) {
		return nil
	}
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})
	if err := r.Status().Patch(ctx, m, patch); err != nil {
		return fmt.Errorf("patching status of %s/%s: %w", m.Namespace, m.Name, err)
	}
	return nil
}

// statusWriter writes a machine's status with an optimistic lock, possibly
// more than once per reconcile. controller-runtime's optimistic-lock patch
// takes the resourceVersion from its *base*, so after a mid-reconcile write
// the base must move forward with it; a second patch from the original base
// would always conflict.
type statusWriter struct {
	r    *Reconciler
	base *pxv1a1.ProxmoxMachine
}

func (r *Reconciler) newStatusWriter(m *pxv1a1.ProxmoxMachine) *statusWriter {
	return &statusWriter{r: r, base: m.DeepCopy()}
}

// flush writes m's status if it changed since the last flush.
//
// A conflict is retried against the object's current resourceVersion, but
// only when the stored status is still the one this reconcile started
// from: then the conflicting write touched something else (usually the
// KubeVM core adding its owner reference). If the status itself moved on,
// this reconcile worked from a stale copy and its decisions (a reserved
// VMID, say) must not overwrite newer ones; the conflict is returned and
// the object reconciled afresh. Without the retry the first reconcile
// nearly always lost the status write that records the clone task.
func (w *statusWriter) flush(ctx context.Context, m *pxv1a1.ProxmoxMachine) error {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		err := w.r.patchStatusIfChanged(ctx, m, w.base)
		if !apierrors.IsConflict(err) {
			return err
		}
		fresh := &pxv1a1.ProxmoxMachine{}
		if gerr := w.r.Get(ctx, client.ObjectKeyFromObject(m), fresh); gerr != nil {
			return gerr
		}
		if !equality.Semantic.DeepEqual(fresh.Status, w.base.Status) {
			return fmt.Errorf("status changed since this reconcile read it: %w", err)
		}
		w.base = fresh.DeepCopy()
		m.ResourceVersion = fresh.ResourceVersion
		return err
	})
	if err != nil {
		return err
	}
	w.base = m.DeepCopy()
	return nil
}

func (r *Reconciler) markNotReady(ctx context.Context, m *pxv1a1.ProxmoxMachine, reason, message string) error {
	base := m.DeepCopy()
	setCondition(m, notReady(reason, message))
	return r.patchStatusIfChanged(ctx, m, base)
}

// setCondition upserts one condition by type.
func setCondition(m *pxv1a1.ProxmoxMachine, c metav1.Condition) {
	c.ObservedGeneration = m.Generation
	m.Status.ObservedGeneration = m.Generation
	conds := &m.Status.Conditions
	for i := range *conds {
		if (*conds)[i].Type == c.Type {
			if (*conds)[i].Status != c.Status {
				c.LastTransitionTime = metav1.Now()
			} else {
				c.LastTransitionTime = (*conds)[i].LastTransitionTime
			}
			(*conds)[i] = c
			return
		}
	}
	c.LastTransitionTime = metav1.Now()
	*conds = append(*conds, c)
}

func ready(reason, message string) metav1.Condition {
	return metav1.Condition{Type: pxv1a1.ConditionInfrastructureReady,
		Status: metav1.ConditionTrue, Reason: reason, Message: message}
}

func notReady(reason, message string) metav1.Condition {
	return metav1.Condition{Type: pxv1a1.ConditionInfrastructureReady,
		Status: metav1.ConditionFalse, Reason: reason, Message: message}
}

func upToDate(ok bool, reason, message string) metav1.Condition {
	s := metav1.ConditionTrue
	if !ok {
		s = metav1.ConditionFalse
	}
	return metav1.Condition{Type: pxv1a1.ConditionUpToDate, Status: s, Reason: reason, Message: message}
}

// SetupWithManager registers the controller and the VirtualMachine watch.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.AllowedPool == "" {
		return errors.New("AllowedPool must be set")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&pxv1a1.ProxmoxMachine{}).
		Watches(&kubevmv1a1.VirtualMachine{},
			handler.EnqueueRequestsFromMapFunc(machineForVirtualMachine)).
		Complete(r)
}

// machineForVirtualMachine maps a VirtualMachine edit to the ProxmoxMachine
// it names.
func machineForVirtualMachine(_ context.Context, o client.Object) []reconcile.Request {
	vm, ok := o.(*kubevmv1a1.VirtualMachine)
	if !ok {
		return nil
	}
	ref := vm.Spec.InfrastructureRef
	if ref.APIGroup != pxv1a1.GroupName || ref.Kind != link.Kind {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: vm.Namespace, Name: ref.Name}}}
}
