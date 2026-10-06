// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains the ProxmoxMachine API, the Proxmox VE half of a
// KubeVM VirtualMachine.
//
// +kubebuilder:object:generate=true
// +groupName=infrastructure.kube-vm.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupName is shared with the other KubeVM reference providers
// (kubevm-provider-container uses the same group for ContainerMachine).
const GroupName = "infrastructure.kube-vm.io"

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: GroupName, Version: "v1alpha1"}

	// SchemeBuilder adds the types in this package to a scheme. Plain
	// apimachinery, not controller-runtime's builder, so importing the API
	// does not pull in controller-runtime.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme adds the types in this group-version to a scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &ProxmoxMachine{}, &ProxmoxMachineList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
