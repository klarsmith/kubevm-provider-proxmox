// SPDX-License-Identifier: Apache-2.0

package proxmoxmachine

import (
	"context"
	"fmt"
	"strings"

	kubevmv1a1 "github.com/vmware-tanzu/vm-operator/external/kubevm/api/v1alpha1"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
)

const mib = 1024 * 1024

// persistResolved copies what the parent asks for into this object's own
// spec, so `kubectl get pxm -o yaml` shows what the controller is acting on.
// Provider-only fields are never touched.
func (r *Reconciler) persistResolved(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	parent *kubevmv1a1.VirtualMachine) error {

	base := m.DeepCopy()
	ps := parent.Spec
	s := &m.Spec

	// The template is fixed once a VM exists: a clone keeps its source.
	if m.Status.VMID == 0 {
		s.Template = ""
		if ps.BootDisk != nil && ps.BootDisk.Source.Image != nil {
			s.Template = ps.BootDisk.Source.Image.Name
		}
	}

	s.PowerState = string(ps.PowerState)
	s.PowerOffMode = string(ps.PowerOffMode)

	s.CPUs, s.MemoryMiB = 0, 0
	if ps.InstanceType != nil && ps.InstanceType.Resources != nil {
		res := ps.InstanceType.Resources
		s.CPUs = res.CPUs
		if res.Memory != nil {
			s.MemoryMiB = (res.Memory.Value() + mib - 1) / mib
		}
	}

	s.BootDiskSizeGiB, s.DeleteOnTermination = 0, nil
	if ps.BootDisk != nil {
		if ps.BootDisk.SizeGiB != nil {
			s.BootDiskSizeGiB = *ps.BootDisk.SizeGiB
		}
		s.DeleteOnTermination = ps.BootDisk.DeleteOnTermination
	}

	s.HostName, s.Nameservers, s.SearchDomains, s.Interfaces = "", nil, nil, nil
	if n := ps.Network; n != nil {
		if n.HostName != nil {
			s.HostName = *n.HostName
		}
		s.Nameservers = n.Nameservers
		s.SearchDomains = n.SearchDomains
		for _, ifc := range n.Interfaces {
			pi := pxv1a1.ProxmoxInterface{Name: ifc.Name, Addresses: ifc.Addresses}
			if ifc.DHCP4 != nil {
				pi.DHCP4 = *ifc.DHCP4
			}
			s.Interfaces = append(s.Interfaces, pi)
		}
	}
	s.SSHPublicKeys = ps.SSHPublicKeys

	return r.patchIfChanged(ctx, m, base)
}

// unsupported lists what the parent asks for that this provider does not
// implement yet. Reported on UpToDate=False/UnsupportedByProvider rather
// than silently ignored.
func unsupported(parent *kubevmv1a1.VirtualMachine) []string {
	ps := parent.Spec
	var out []string
	if ps.PowerState == kubevmv1a1.PowerStateSuspended {
		out = append(out, "powerState Suspended")
	}
	if ps.InstanceType != nil && ps.InstanceType.Name != "" {
		out = append(out, fmt.Sprintf("instanceType.name %q (use instanceType.resources)", ps.InstanceType.Name))
	}
	if b := ps.BootDisk; b != nil {
		if b.Source.Snapshot != nil || b.Source.Blank {
			out = append(out, "bootDisk sources other than image")
		}
		if b.StorageClassName != "" {
			out = append(out, "bootDisk.storageClassName (set spec.storage on the ProxmoxMachine)")
		}
		if b.VolumeAttributesClassName != "" {
			out = append(out, "bootDisk.volumeAttributesClassName")
		}
		if b.DeleteOnTermination != nil && !*b.DeleteOnTermination {
			out = append(out, "bootDisk.deleteOnTermination=false")
		}
	}
	if len(ps.Disks) > 0 {
		out = append(out, "data disks")
	}
	if n := ps.Network; n != nil {
		for _, ifc := range n.Interfaces {
			if ifc.Network != nil {
				out = append(out, fmt.Sprintf("interface %q network reference", ifc.Name))
			}
			if ifc.PublicIP != nil && *ifc.PublicIP {
				out = append(out, fmt.Sprintf("interface %q publicIP", ifc.Name))
			}
			if ifc.DHCP6 != nil && *ifc.DHCP6 {
				out = append(out, fmt.Sprintf("interface %q dhcp6", ifc.Name))
			}
			for _, a := range ifc.Addresses {
				if strings.Contains(a, ":") {
					out = append(out, fmt.Sprintf("interface %q static IPv6 address", ifc.Name))
					break
				}
			}
		}
	}
	if b := ps.Bootstrap; b != nil && b.CloudInit != nil &&
		(b.CloudInit.UserData != nil || b.CloudInit.NetworkData != nil) {
		out = append(out, "bootstrap.cloudInit userData/networkData")
	}
	if ps.FailureDomain != nil && *ps.FailureDomain != "" {
		out = append(out, "failureDomain")
	}
	if ps.Scheduling != nil && ps.Scheduling.Spot != nil && *ps.Scheduling.Spot {
		out = append(out, "scheduling.spot")
	}
	if len(ps.Tags) > 0 {
		out = append(out, "tags")
	}
	return out
}
