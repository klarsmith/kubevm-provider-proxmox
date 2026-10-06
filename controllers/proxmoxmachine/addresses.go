// SPDX-License-Identifier: Apache-2.0

package proxmoxmachine

import (
	"context"
	"fmt"
	"net"
	"strings"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
)

// addresses reads guest addresses from the QEMU guest agent. Loopback and
// link-local addresses are dropped. Each guest NIC is matched to a Proxmox
// netN by MAC, so the address carries the portable interface name where
// one was declared, and the guest's own NIC name otherwise.
func (r *Reconciler) addresses(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vm *proxmox.VM) ([]pxv1a1.ProxmoxMachineAddress, error) {

	ifaces, err := px.AgentInterfaces(ctx, vm.Node, vm.VMID)
	if err != nil {
		return nil, err
	}
	cfg, err := px.Config(ctx, vm.Node, vm.VMID)
	if err != nil {
		return nil, fmt.Errorf("reading config of VM %d: %w", vm.VMID, err)
	}
	names := portableNamesByMAC(m, cfg)

	var out []pxv1a1.ProxmoxMachineAddress
	for _, ifc := range ifaces {
		name := ifc.Name
		if n, ok := names[ifc.MAC]; ok {
			name = n
		}
		for _, ip := range ifc.IPs {
			parsed := net.ParseIP(ip.Address)
			if parsed == nil || parsed.IsLoopback() || parsed.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, pxv1a1.ProxmoxMachineAddress{
				Interface: name,
				Type:      pxv1a1.AddressInternalIP,
				Address:   ip.Address,
			})
		}
	}
	return out, nil
}

// portableNamesByMAC maps the MAC of netN to spec.interfaces[N].name.
func portableNamesByMAC(m *pxv1a1.ProxmoxMachine, cfg map[string]string) map[string]string {
	out := map[string]string{}
	for i, ifc := range m.Spec.Interfaces {
		if mac := macOf(cfg[fmt.Sprintf("net%d", i)]); mac != "" {
			out[mac] = ifc.Name
		}
	}
	return out
}

// macOf extracts the MAC from a netN value like
// "virtio=BC:24:11:00:00:01,bridge=vmbr0".
func macOf(netValue string) string {
	model, _, _ := strings.Cut(netValue, ",")
	_, mac, ok := strings.Cut(model, "=")
	if !ok {
		return ""
	}
	return strings.ToLower(mac)
}
