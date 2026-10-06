// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Finalizer holds a ProxmoxMachine until its Proxmox VM has been destroyed.
const Finalizer = "infrastructure.kube-vm.io/proxmoxmachine"

// AnnotationKey names the VirtualMachine this object belongs to. Adoption
// needs both sides: this annotation AND the VirtualMachine's
// spec.infrastructureRef naming this object back.
const AnnotationKey = "kube-vm.io/virtual-machine"

// DefaultPool is the Proxmox pool the controller is allowed to act in unless
// the manager is started with a different --allowed-pool.
const DefaultPool = "kubevm-dev"

// ProxmoxInterface is one guest network interface, resolved from the
// portable spec.network.interfaces.
type ProxmoxInterface struct {
	// Name is the portable interface name. Proxmox has no per-NIC name, so
	// it is only used to report addresses back; the NIC is netN by position.
	Name string `json:"name"`

	// Addresses are bare IPs, as on the portable object. Combined with
	// spec.staticIPPrefixLength and spec.gateway to build ipconfigN.
	// +optional
	Addresses []string `json:"addresses,omitempty"`

	// DHCP4 requests DHCP for IPv4. Also the behaviour when no address is
	// given.
	// +optional
	DHCP4 bool `json:"dhcp4,omitempty"`
}

// ProxmoxMachineSpec is the Proxmox-specific half of a machine.
//
// Two kinds of field live here. "Resolved" fields are copied from the parent
// VirtualMachine by the controller on every reconcile; edit the
// VirtualMachine, not these. "Provider-only" fields have no portable
// equivalent; whoever creates this object sets them once and the controller
// never writes them.
type ProxmoxMachineSpec struct {
	// Resolved. The name of the Proxmox template VM to clone, read verbatim
	// from spec.bootDisk.source.image.name. Fixed once a VM exists.
	// +optional
	Template string `json:"template,omitempty"`

	// Resolved. The power state asked for on the parent.
	// +kubebuilder:validation:Enum=PoweredOn;PoweredOff;Suspended
	// +optional
	PowerState string `json:"powerState,omitempty"`

	// Resolved. How a power-off is performed.
	// +kubebuilder:validation:Enum=Hard;Soft;TrySoft
	// +optional
	PowerOffMode string `json:"powerOffMode,omitempty"`

	// Resolved. From spec.instanceType.resources.cpus. Zero keeps the
	// template's value.
	// +optional
	CPUs int32 `json:"cpus,omitempty"`

	// Resolved. From spec.instanceType.resources.memory, rounded up to MiB.
	// Zero keeps the template's value.
	// +optional
	MemoryMiB int64 `json:"memoryMiB,omitempty"`

	// Resolved. From spec.bootDisk.sizeGiB. The boot disk is only grown,
	// never shrunk.
	// +optional
	BootDiskSizeGiB int64 `json:"bootDiskSizeGiB,omitempty"`

	// Resolved. From spec.bootDisk.deleteOnTermination. Nil means true.
	// +optional
	DeleteOnTermination *bool `json:"deleteOnTermination,omitempty"`

	// Resolved. From spec.network.hostName. Becomes the Proxmox VM name,
	// which Proxmox cloud-init uses as the guest hostname.
	// +optional
	HostName string `json:"hostName,omitempty"`

	// Resolved. From spec.network.nameservers.
	// +optional
	Nameservers []string `json:"nameservers,omitempty"`

	// Resolved. From spec.network.searchDomains.
	// +optional
	SearchDomains []string `json:"searchDomains,omitempty"`

	// Resolved. From spec.sshPublicKeys.
	// +optional
	SSHPublicKeys []string `json:"sshPublicKeys,omitempty"`

	// Resolved. From spec.network.interfaces.
	// +optional
	Interfaces []ProxmoxInterface `json:"interfaces,omitempty"`

	// Provider-only. A Secret in this namespace with keys url, tokenID,
	// tokenSecret and optionally caBundle and insecureSkipVerify ("true").
	CredentialsSecretRef corev1.LocalObjectReference `json:"credentialsSecretRef"`

	// Provider-only. Node to place the clone on. Empty means the template's
	// node. Only used at creation; the VM's current node is always looked up
	// fresh, because HA and migration move VMs.
	// +optional
	Node string `json:"node,omitempty"`

	// Provider-only. Storage for the cloned disks (full clones only). Empty
	// means the template's storage.
	// +optional
	Storage string `json:"storage,omitempty"`

	// Provider-only. Proxmox pool the VM is created in. Must equal the pool
	// the manager is allowed to act in.
	// +kubebuilder:default=kubevm-dev
	// +optional
	Pool string `json:"pool,omitempty"`

	// Provider-only. Full clone (true) or linked clone (false).
	// +kubebuilder:default=true
	// +optional
	FullClone *bool `json:"fullClone,omitempty"`

	// Provider-only. Bridge for every interface in spec.interfaces.
	// +kubebuilder:default=vmbr0
	// +optional
	Bridge string `json:"bridge,omitempty"`

	// Provider-only. Prefix length for static addresses. KubeVM addresses
	// are bare IPs and Proxmox's ipconfigN needs a CIDR.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	// +optional
	StaticIPPrefixLength int32 `json:"staticIPPrefixLength,omitempty"`

	// Provider-only. IPv4 gateway for static addresses.
	// +optional
	Gateway string `json:"gateway,omitempty"`

	// Provider-only. cloud-init user (ciuser). Empty keeps the template's.
	// +optional
	CIUser string `json:"ciUser,omitempty"`
}

// AddressInternalIP is the only address type this provider reports.
const AddressInternalIP = "InternalIP"

// ProxmoxMachineAddress is one observed guest address.
type ProxmoxMachineAddress struct {
	// +optional
	Interface string `json:"interface,omitempty"`

	// +kubebuilder:validation:Enum=InternalIP
	Type string `json:"type"`

	// +kubebuilder:validation:MinLength=1
	Address string `json:"address"`
}

// ProxmoxMachineStatus carries the KubeVM contract paths (addresses,
// powerState, providerID, providerMetadata, conditions) plus this
// provider's own bookkeeping.
type ProxmoxMachineStatus struct {
	// +optional
	// +listType=atomic
	Addresses []ProxmoxMachineAddress `json:"addresses,omitempty"`

	// Observed power state. Absent while a power operation is in flight.
	// +kubebuilder:validation:Enum=PoweredOn;PoweredOff
	// +optional
	PowerState string `json:"powerState,omitempty"`

	// proxmox://<cluster>/<vmid>. No node: VMs move between nodes.
	// +optional
	ProviderID string `json:"providerID,omitempty"`

	// +optional
	ProviderMetadata map[string]string `json:"providerMetadata,omitempty"`

	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// VMID of the Proxmox VM, recorded as soon as the clone call returns.
	// +optional
	VMID int `json:"vmid,omitempty"`

	// PendingTask is the UPID of the Proxmox task in flight, if any.
	// +optional
	PendingTask string `json:"pendingTask,omitempty"`

	// PendingOp says what PendingTask is doing.
	// +optional
	PendingOp string `json:"pendingOp,omitempty"`

	// Provisioned is set once the pre-boot configuration (sizing,
	// cloud-init, network, disk resize) has been applied.
	// +optional
	Provisioned bool `json:"provisioned,omitempty"`

	// ConsecutiveFailures counts Proxmox tasks that failed in a row. Reset
	// by the next task that succeeds.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// LastFailure is the most recent failed task's operation and exit
	// status.
	// +optional
	LastFailure string `json:"lastFailure,omitempty"`

	// NextAttempt is when the controller may start another Proxmox task
	// after a failure. It grows exponentially with ConsecutiveFailures, so a
	// VM that cannot start is not hammered with start calls.
	// +optional
	NextAttempt *metav1.Time `json:"nextAttempt,omitempty"`
}

// Operations recorded in status.pendingOp.
const (
	OpClone    = "Clone"
	OpResize   = "Resize"
	OpStart    = "Start"
	OpShutdown = "Shutdown"
	OpStop     = "Stop"
	OpDelete   = "Delete"
)

// Condition types read by the KubeVM core.
const (
	ConditionInfrastructureReady = "InfrastructureReady"
	ConditionUpToDate            = "UpToDate"
)

// Condition reasons.
const (
	ReasonNotAdopted            = "NotAdopted"
	ReasonAlreadyOwned          = "AlreadyOwned"
	ReasonInvalidConfiguration  = "InvalidConfiguration"
	ReasonUnsupportedByProvider = "UnsupportedByProvider"
	ReasonProvisioning          = "Provisioning"
	ReasonRunning               = "Running"
	ReasonStopped               = "Stopped"
	ReasonPowerChanging         = "PowerChanging"
	ReasonApplied               = "Applied"
	ReasonTaskRunning           = "TaskRunning"
	ReasonTaskFailed            = "TaskFailed"
	ReasonAPIError              = "APIError"
	ReasonVMGone                = "VMGone"
	ReasonOutsidePool           = "OutsidePool"
	ReasonDeleting              = "Deleting"
	ReasonPaused                = "Paused"
)

// +kubebuilder:metadata:labels="kube-vm.io/v1alpha1=v1alpha1"
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=pxm,categories=kubevm
// +kubebuilder:printcolumn:name="VMID",type=integer,JSONPath=`.status.vmid`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.powerState`
// +kubebuilder:printcolumn:name="Template",type=string,JSONPath=`.spec.template`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="InfrastructureReady")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ProxmoxMachine is one Proxmox VE QEMU VM backing one KubeVM VirtualMachine.
type ProxmoxMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProxmoxMachineSpec   `json:"spec,omitempty"`
	Status ProxmoxMachineStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProxmoxMachineList is a list of ProxmoxMachine.
type ProxmoxMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []ProxmoxMachine `json:"items"`
}
