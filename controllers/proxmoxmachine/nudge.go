// SPDX-License-Identifier: Apache-2.0

package proxmoxmachine

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kubevmv1a1 "github.com/vmware-tanzu/vm-operator/external/kubevm/api/v1alpha1"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
)

// StatusHashAnnotation is stamped on the parent VirtualMachine with a hash
// of the contract status this provider last published.
//
// Why: the KubeVM core does not watch provider objects. It re-reads them
// when the VirtualMachine changes, and polls only while the provider is
// neither ready nor reporting addresses. A change that happens after
// readiness (a power-off finishing, an address appearing, the VM vanishing)
// would never reach VirtualMachine.status. The core's VirtualMachine watch
// has no predicates, so an annotation change on the parent is enough to make
// it re-read. See docs/findings.md, "The core does not see status changes
// after readiness". The proper fix is proposed upstream as
// https://github.com/vmware-tanzu/vm-operator/pull/2008; once that lands
// and the KubeVM pin is bumped, this file can go.
const StatusHashAnnotation = "infrastructure.kube-vm.io/provider-status-hash"

// contractHash hashes exactly the status paths the core reads.
func contractHash(m *pxv1a1.ProxmoxMachine) string {
	type cond struct{ Type, Status, Reason, Message string }
	var conds []cond
	for _, c := range m.Status.Conditions {
		if c.Type == pxv1a1.ConditionInfrastructureReady || c.Type == pxv1a1.ConditionUpToDate {
			conds = append(conds, cond{c.Type, string(c.Status), c.Reason, c.Message})
		}
	}
	b, _ := json.Marshal(struct {
		A any
		P string
		I string
		M map[string]string
		C []cond
	}{m.Status.Addresses, m.Status.PowerState, m.Status.ProviderID, m.Status.ProviderMetadata, conds})
	h := fnv.New64a()
	_, _ = h.Write(b)
	return fmt.Sprintf("%016x", h.Sum64())
}

// nudgeParent stamps the current contract hash on the parent if it differs.
func (r *Reconciler) nudgeParent(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	parent *kubevmv1a1.VirtualMachine) error {

	hash := contractHash(m)
	if parent.Annotations[StatusHashAnnotation] == hash {
		return nil
	}
	base := parent.DeepCopy()
	if parent.Annotations == nil {
		parent.Annotations = map[string]string{}
	}
	parent.Annotations[StatusHashAnnotation] = hash
	if err := r.Patch(ctx, parent, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("annotating VirtualMachine %s/%s: %w", parent.Namespace, parent.Name, err)
	}
	return nil
}
