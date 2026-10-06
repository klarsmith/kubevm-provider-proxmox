// SPDX-License-Identifier: Apache-2.0

// Package controllers adds every controller this manager runs.
package controllers

import (
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	kubevmcore "github.com/vmware-tanzu/vm-operator/external/kubevm/controller/controllers/virtualmachine"

	"github.com/klarsmith/kubevm-provider-proxmox/controllers/proxmoxmachine"
)

// +kubebuilder:rbac:groups=kube-vm.io,resources=virtualmachines,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=kube-vm.io,resources=virtualmachines/status,verbs=get;patch
// +kubebuilder:rbac:groups=kube-vm.io,resources=virtualmachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=infrastructure.kube-vm.io,resources=proxmoxmachines,verbs=delete

// Options configures the controllers.
type Options struct {
	// AllowedPool is the only Proxmox pool VMs are created, changed or
	// deleted in.
	AllowedPool string

	// NewProxmoxClient overrides the Proxmox client (tests). Nil means the
	// real HTTP client.
	NewProxmoxClient proxmoxmachine.ClientFactory

	// PollInterval overrides the requeue delay for in-flight tasks (tests).
	PollInterval time.Duration
}

// AddToManager adds the ProxmoxMachine controller and the KubeVM core
// controller to one manager: one process, one cache, both API groups.
func AddToManager(mgr ctrl.Manager, opts Options) error {
	r := &proxmoxmachine.Reconciler{
		Client:           mgr.GetClient(),
		AllowedPool:      opts.AllowedPool,
		NewProxmoxClient: opts.NewProxmoxClient,
		PollInterval:     opts.PollInterval,
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("adding the ProxmoxMachine controller: %w", err)
	}
	if err := kubevmcore.AddToManager(mgr); err != nil {
		return fmt.Errorf("adding the KubeVM core controller: %w", err)
	}
	return nil
}
