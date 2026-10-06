// SPDX-License-Identifier: Apache-2.0

package proxmoxmachine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
)

const gib = int64(1024 * 1024 * 1024)

// provision applies the pre-boot configuration to a freshly cloned, stopped
// VM: sizing, native cloud-init fields, NICs, and the boot disk size. done
// is false while a resize task is in flight.
func (r *Reconciler) provision(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vm *proxmox.VM) (ctrl.Result, bool, error) {

	cfg, err := px.Config(ctx, vm.Node, vm.VMID)
	if err != nil {
		return ctrl.Result{}, false, fmt.Errorf("reading config of VM %d: %w", vm.VMID, err)
	}

	params := sizingParams(m, cfg)
	for k, v := range cloudInitParams(m, cfg) {
		params[k] = v
	}
	if !hasTag(cfg["tags"], "kubevm") {
		params.Set("tags", joinTags(cfg["tags"], "kubevm"))
	}
	if len(params) > 0 {
		if err := px.SetConfig(ctx, vm.Node, vm.VMID, params); errors.Is(err, proxmox.ErrInvalidParameter) {
			// The VM cannot be provisioned as asked. Reported, not retried:
			// only a spec change can fix it, and that triggers a reconcile.
			setCondition(m, notReady(pxv1a1.ReasonInvalidConfiguration,
				fmt.Sprintf("Proxmox rejected the configuration for VM %d: %v", vm.VMID, err)))
			setCondition(m, upToDate(false, pxv1a1.ReasonInvalidConfiguration, err.Error()))
			return ctrl.Result{RequeueAfter: resyncPeriod}, false, nil
		} else if err != nil {
			return ctrl.Result{}, false, fmt.Errorf("configuring VM %d: %w", vm.VMID, err)
		}
	}

	upid, err := r.growBootDisk(ctx, m, px, vm, cfg)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	if upid != "" {
		return ctrl.Result{RequeueAfter: r.pollDelay()}, false, nil
	}

	m.Status.Provisioned = true
	return ctrl.Result{}, true, nil
}

// applyDrift reconciles what may change after first boot: sizing (only
// while stopped) and boot disk growth (any time). Returns notes for UpToDate.
func (r *Reconciler) applyDrift(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vm *proxmox.VM) ([]string, ctrl.Result, error) {

	cfg, err := px.Config(ctx, vm.Node, vm.VMID)
	if err != nil {
		return nil, ctrl.Result{}, fmt.Errorf("reading config of VM %d: %w", vm.VMID, err)
	}

	var notes []string
	// cloud-init settings and the VM name can be written any time; the
	// guest picks them up at its next boot.
	ci := cloudInitParams(m, cfg)
	if name := vmName(m); cfg["name"] != name {
		ci.Set("name", name)
	}
	if len(ci) > 0 {
		switch err := px.SetConfig(ctx, vm.Node, vm.VMID, ci); {
		case errors.Is(err, proxmox.ErrInvalidParameter):
			// A key or option Proxmox cannot accept: a configuration error
			// on UpToDate, not an API failure to retry.
			notes = append(notes, "rejected by Proxmox: "+err.Error())
		case err != nil:
			return nil, ctrl.Result{}, fmt.Errorf("updating cloud-init of VM %d: %w", vm.VMID, err)
		case vm.Status != "stopped":
			notes = append(notes, "cloud-init/network changes take effect at the next boot")
		}
	}
	if params := sizingParams(m, cfg); len(params) > 0 {
		if vm.Status == "stopped" {
			if err := px.SetConfig(ctx, vm.Node, vm.VMID, params); err != nil {
				return nil, ctrl.Result{}, fmt.Errorf("resizing VM %d: %w", vm.VMID, err)
			}
		} else {
			notes = append(notes, "cpus/memory change is applied the next time the VM is powered off")
		}
	}

	upid, err := r.growBootDisk(ctx, m, px, vm, cfg)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if upid != "" {
		return nil, ctrl.Result{RequeueAfter: r.pollDelay()}, nil
	}
	if want := m.Spec.BootDiskSizeGiB; want > 0 {
		if _, size, ok := bootDisk(cfg); ok && size > want*gib {
			notes = append(notes, fmt.Sprintf(
				"bootDisk.sizeGiB %d is smaller than the disk; disks are only grown", want))
		}
	}
	return notes, ctrl.Result{}, nil
}

// growBootDisk starts a resize if the boot disk is smaller than asked for.
// Returns the task UPID, or "" if nothing was started (or Proxmox resized
// synchronously).
func (r *Reconciler) growBootDisk(ctx context.Context, m *pxv1a1.ProxmoxMachine,
	px proxmox.Client, vm *proxmox.VM, cfg map[string]string) (string, error) {

	want := m.Spec.BootDiskSizeGiB
	if want <= 0 {
		return "", nil
	}
	disk, size, ok := bootDisk(cfg)
	if !ok {
		return "", fmt.Errorf("cannot find the boot disk of VM %d in its config", vm.VMID)
	}
	if size >= want*gib {
		return "", nil
	}
	upid, err := px.Resize(ctx, vm.Node, vm.VMID, disk, fmt.Sprintf("%dG", want))
	if err != nil {
		return "", fmt.Errorf("growing %s of VM %d to %dG: %w", disk, vm.VMID, want, err)
	}
	if upid != "" {
		m.Status.PendingTask, m.Status.PendingOp = upid, pxv1a1.OpResize
		setCondition(m, notReady(pxv1a1.ReasonProvisioning,
			fmt.Sprintf("growing %s of VM %d to %dG", disk, vm.VMID, want)))
	}
	return upid, nil
}

// sizingParams returns cores/memory settings that differ from cfg.
func sizingParams(m *pxv1a1.ProxmoxMachine, cfg map[string]string) url.Values {
	p := url.Values{}
	if c := m.Spec.CPUs; c > 0 && cfg["cores"] != strconv.Itoa(int(c)) {
		p.Set("cores", strconv.Itoa(int(c)))
	}
	if mem := m.Spec.MemoryMiB; mem > 0 && cfg["memory"] != strconv.FormatInt(mem, 10) {
		p.Set("memory", strconv.FormatInt(mem, 10))
	}
	return p
}

// cloudInitParams maps the portable network and SSH fields onto Proxmox's
// native cloud-init options. Only values that differ from cfg are returned.
func cloudInitParams(m *pxv1a1.ProxmoxMachine, cfg map[string]string) url.Values {
	s := m.Spec
	want := map[string]string{}
	if s.CIUser != "" {
		want["ciuser"] = s.CIUser
	}
	if len(s.SSHPublicKeys) > 0 {
		want["sshkeys"] = encodeSSHKeys(s.SSHPublicKeys)
	}
	if len(s.Nameservers) > 0 {
		want["nameserver"] = strings.Join(s.Nameservers, " ")
	}
	if len(s.SearchDomains) > 0 {
		want["searchdomain"] = strings.Join(s.SearchDomains, " ")
	}
	for i, ifc := range s.Interfaces {
		key := fmt.Sprintf("net%d", i)
		want[key] = withBridge(cfg[key], s.Bridge)
		want[fmt.Sprintf("ipconfig%d", i)] = ipConfig(ifc, s.StaticIPPrefixLength, s.Gateway)
	}

	p := url.Values{}
	for k, v := range want {
		if cfg[k] != v {
			p.Set(k, v)
		}
	}
	return p
}

// encodeSSHKeys URL-encodes the keys as Proxmox's sshkeys option requires:
// the stored value is percent-encoded, spaces as %20 (not "+"), and the
// whole thing is then form-encoded again on the wire.
func encodeSSHKeys(keys []string) string {
	return strings.ReplaceAll(url.QueryEscape(strings.Join(keys, "\n")), "+", "%20")
}

// withBridge keeps an existing NIC's model and MAC and only sets its bridge;
// a NIC the template does not have gets a virtio model and a new MAC.
func withBridge(existing, bridge string) string {
	if existing == "" {
		return "virtio,bridge=" + bridge
	}
	parts := strings.Split(existing, ",")
	found := false
	for i, p := range parts {
		if strings.HasPrefix(p, "bridge=") {
			parts[i] = "bridge=" + bridge
			found = true
		}
	}
	if !found {
		parts = append(parts, "bridge="+bridge)
	}
	return strings.Join(parts, ",")
}

// ipConfig builds ipconfigN. KubeVM addresses are bare IPs, so the prefix
// length and gateway come from the provider-only fields.
func ipConfig(ifc pxv1a1.ProxmoxInterface, prefix int32, gateway string) string {
	for _, a := range ifc.Addresses {
		if strings.Contains(a, ":") || prefix == 0 {
			continue // IPv6 is not supported yet; validated elsewhere
		}
		s := fmt.Sprintf("ip=%s/%d", a, prefix)
		if gateway != "" {
			s += ",gw=" + gateway
		}
		return s
	}
	return "ip=dhcp"
}

// bootDisk finds the boot disk key and its size in bytes. It reads the
// boot order rather than assuming scsi0, because templates differ.
func bootDisk(cfg map[string]string) (key string, size int64, ok bool) {
	var candidates []string
	if order, found := strings.CutPrefix(cfg["boot"], "order="); found {
		candidates = strings.Split(order, ";")
	}
	if d := cfg["bootdisk"]; d != "" { // pre-PVE 6.2 style
		candidates = append(candidates, d)
	}
	var keys []string
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, prefix := range []string{"scsi", "virtio", "sata", "ide"} {
		for _, k := range keys {
			if strings.HasPrefix(k, prefix) {
				candidates = append(candidates, k)
			}
		}
	}
	for _, k := range candidates {
		v := cfg[k]
		if v == "" || strings.Contains(v, "media=cdrom") || strings.Contains(v, "cloudinit") {
			continue
		}
		if sz, found := diskSize(v); found {
			return k, sz, true
		}
	}
	return "", 0, false
}

// diskSize parses the size= option of a disk value like
// "local-lvm:vm-100-disk-0,size=8G".
func diskSize(v string) (int64, bool) {
	for _, p := range strings.Split(v, ",") {
		s, ok := strings.CutPrefix(p, "size=")
		if !ok || s == "" {
			continue
		}
		mult := int64(1)
		switch s[len(s)-1] {
		case 'K':
			mult = 1024
		case 'M':
			mult = 1024 * 1024
		case 'G':
			mult = gib
		case 'T':
			mult = gib * 1024
		}
		if mult != 1 {
			s = s[:len(s)-1]
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return int64(n * float64(mult)), true
	}
	return 0, false
}

func hasTag(tags, tag string) bool {
	for _, t := range strings.FieldsFunc(tags, func(r rune) bool { return r == ';' || r == ',' || r == ' ' }) {
		if t == tag {
			return true
		}
	}
	return false
}

func joinTags(existing, tag string) string {
	if existing == "" {
		return tag
	}
	return existing + ";" + tag
}
