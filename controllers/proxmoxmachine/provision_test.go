// SPDX-License-Identifier: Apache-2.0

package proxmoxmachine

import (
	"testing"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
)

func TestBootDisk(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      map[string]string
		wantKey  string
		wantSize int64
	}{
		{"boot order", map[string]string{
			"boot": "order=virtio0;ide2;net0", "virtio0": "local:vm-1-disk-0,size=8G",
			"scsi0": "local:vm-1-disk-1,size=100G", "ide2": "local:cloudinit,media=cdrom",
		}, "virtio0", 8 * gib},
		{"legacy bootdisk", map[string]string{
			"bootdisk": "scsi1", "scsi1": "ceph:vm-1-disk-0,size=2048M",
		}, "scsi1", 2 * gib},
		{"no boot order", map[string]string{
			"ide2": "local-lvm:vm-1-cloudinit,media=cdrom", "scsi0": "local-lvm:vm-1-disk-0,size=1.5T",
		}, "scsi0", 1536 * gib},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k, sz, ok := bootDisk(tc.cfg)
			if !ok || k != tc.wantKey || sz != tc.wantSize {
				t.Errorf("bootDisk = %q %d %v, want %q %d", k, sz, ok, tc.wantKey, tc.wantSize)
			}
		})
	}
}

func TestEncodeSSHKeys(t *testing.T) {
	got := encodeSSHKeys([]string{"ssh-ed25519 AAA+b/c= a@b", "ssh-rsa BBB c@d"})
	want := "ssh-ed25519%20AAA%2Bb%2Fc%3D%20a%40b%0Assh-rsa%20BBB%20c%40d"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestIPConfig(t *testing.T) {
	for _, tc := range []struct {
		ifc    pxv1a1.ProxmoxInterface
		prefix int32
		gw     string
		want   string
	}{
		{pxv1a1.ProxmoxInterface{}, 0, "", "ip=dhcp"},
		{pxv1a1.ProxmoxInterface{DHCP4: true}, 24, "", "ip=dhcp"},
		{pxv1a1.ProxmoxInterface{Addresses: []string{"10.1.2.3"}}, 16, "", "ip=10.1.2.3/16"},
		{pxv1a1.ProxmoxInterface{Addresses: []string{"fd00::5", "10.1.2.3"}}, 24, "10.1.2.1", "ip=10.1.2.3/24,gw=10.1.2.1"},
	} {
		if got := ipConfig(tc.ifc, tc.prefix, tc.gw); got != tc.want {
			t.Errorf("ipConfig(%+v) = %q, want %q", tc.ifc, got, tc.want)
		}
	}
}

func TestWithBridge(t *testing.T) {
	for in, want := range map[string]string{
		"":                                      "virtio,bridge=vmbr1",
		"virtio=BC:24:11:AA:BB:CC,bridge=vmbr0": "virtio=BC:24:11:AA:BB:CC,bridge=vmbr1",
		"e1000=BC:24:11:AA:BB:CC,firewall=1":    "e1000=BC:24:11:AA:BB:CC,firewall=1,bridge=vmbr1",
	} {
		if got := withBridge(in, "vmbr1"); got != want {
			t.Errorf("withBridge(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMacOf(t *testing.T) {
	if got := macOf("virtio=BC:24:11:AA:BB:CC,bridge=vmbr0"); got != "bc:24:11:aa:bb:cc" {
		t.Errorf("macOf = %q", got)
	}
	if got := macOf("virtio,bridge=vmbr0"); got != "" {
		t.Errorf("macOf without MAC = %q", got)
	}
}
