// SPDX-License-Identifier: Apache-2.0

// Package link performs the two-sided adoption check: a ProxmoxMachine is
// only acted on when its annotation names a VirtualMachine AND that
// VirtualMachine's spec.infrastructureRef names this object back. Same
// pattern as kubevm-provider-container's internal/link.
package link

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kubevmv1a1 "github.com/vmware-tanzu/vm-operator/external/kubevm/api/v1alpha1"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
)

// Kind is the kind a VirtualMachine's infrastructureRef must name.
const Kind = "ProxmoxMachine"

// ErrNotLinked means the object carries no back-reference annotation.
var ErrNotLinked = errors.New("no back-reference annotation")

// ErrParentMissing means the annotation names a VirtualMachine that does not
// exist.
var ErrParentMissing = errors.New("named VirtualMachine does not exist")

// ErrNotMutual means the named VirtualMachine does not name this object.
var ErrNotMutual = errors.New("VirtualMachine does not name this object")

// ParentOf returns the VirtualMachine this object is mutually linked to, or
// one of the sentinel errors above.
func ParentOf(ctx context.Context, c client.Client, obj client.Object) (
	*kubevmv1a1.VirtualMachine, error) {

	name := obj.GetAnnotations()[pxv1a1.AnnotationKey]
	if name == "" {
		return nil, ErrNotLinked
	}

	vm := &kubevmv1a1.VirtualMachine{}
	key := client.ObjectKey{Namespace: obj.GetNamespace(), Name: name}
	if err := c.Get(ctx, key, vm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrParentMissing
		}
		return nil, fmt.Errorf("getting VirtualMachine %q: %w", name, err)
	}

	if !Names(vm, obj.GetName()) {
		return nil, ErrNotMutual
	}
	return vm, nil
}

// Names reports whether vm's infrastructureRef names the ProxmoxMachine
// called name.
func Names(vm *kubevmv1a1.VirtualMachine, name string) bool {
	ref := vm.Spec.InfrastructureRef
	return ref.APIGroup == pxv1a1.GroupName && ref.Kind == Kind && ref.Name == name
}
