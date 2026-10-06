// SPDX-License-Identifier: Apache-2.0

package controllers_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kubevmv1a1 "github.com/vmware-tanzu/vm-operator/external/kubevm/api/v1alpha1"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
	"github.com/klarsmith/kubevm-provider-proxmox/controllers"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox"
	"github.com/klarsmith/kubevm-provider-proxmox/internal/proxmox/proxmoxfake"
)

// TestEndToEnd runs the real manager (KubeVM core + ProxmoxMachine
// controller) against a real API server, with the in-memory Proxmox, and
// drives a machine through create, power off/on, and delete using only the
// portable VirtualMachine.
func TestEndToEnd(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run `make test` for envtest")
	}
	g := NewWithT(t)
	ctrl.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(&testWriter{t})))

	testEnv := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "config", "crd", "bases"),
			filepath.Join("..", "config", "crd", "kubevm"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() { _ = testEnv.Stop() })

	scheme := runtime.NewScheme()
	g.Expect(clientgoscheme.AddToScheme(scheme)).To(Succeed())
	g.Expect(pxv1a1.AddToScheme(scheme)).To(Succeed())
	g.Expect(kubevmv1a1.AddToScheme(scheme)).To(Succeed())

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	g.Expect(err).NotTo(HaveOccurred())

	px := proxmoxfake.New()
	px.TaskPolls = 0
	g.Expect(controllers.AddToManager(mgr, controllers.Options{
		AllowedPool:      pxv1a1.DefaultPool,
		NewProxmoxClient: func(proxmox.Credentials) (proxmox.Client, error) { return px, nil },
		PollInterval:     200 * time.Millisecond,
	})).To(Succeed())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = mgr.Start(ctx) }()

	c := mgr.GetClient()
	const ns, name = "default", "web-01"
	key := client.ObjectKey{Namespace: ns, Name: name}

	g.Expect(c.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "pve"},
		StringData: map[string]string{"url": "https://pve.test:8006", "tokenID": "a@pve!b", "tokenSecret": "c"},
	})).To(Succeed())

	g.Expect(c.Create(ctx, &kubevmv1a1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: kubevmv1a1.VirtualMachineSpec{
			InfrastructureRef: kubevmv1a1.ObjectReference{
				APIGroup: pxv1a1.GroupName, Kind: "ProxmoxMachine", Name: name,
			},
			PowerState: kubevmv1a1.PowerStateOn,
			BootDisk: &kubevmv1a1.BootDiskSpec{
				Source: kubevmv1a1.DiskSource{Image: &kubevmv1a1.ObjectReference{
					APIGroup: pxv1a1.GroupName, Kind: "ProxmoxTemplate", Name: "debian-12",
				}},
				SizeGiB: ptr.To[int64](10),
			},
		},
	})).To(Succeed())

	// Defaults (pool, bridge, fullClone) come from the CRD schema here,
	// unlike in the unit tests.
	g.Expect(c.Create(ctx, &pxv1a1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
			Annotations: map[string]string{pxv1a1.AnnotationKey: name}},
		Spec: pxv1a1.ProxmoxMachineSpec{CredentialsSecretRef: corev1.LocalObjectReference{Name: "pve"}},
	})).To(Succeed())

	vm := &kubevmv1a1.VirtualMachine{}
	getVM := func() *kubevmv1a1.VirtualMachine {
		g.Expect(c.Get(ctx, key, vm)).To(Succeed())
		return vm
	}

	// The core mirrors readiness on its own ~10s poll of the provider object.
	g.Eventually(getVM, 60*time.Second, 250*time.Millisecond).Should(
		WithTransform(func(v *kubevmv1a1.VirtualMachine) bool {
			return v.Status.Ready && v.Status.PowerState == kubevmv1a1.PowerStateOn &&
				len(v.Status.Addresses) > 0 && v.Status.ProviderID == "proxmox://testcluster/100"
		}, BeTrue()), "VirtualMachine never became ready")

	pm := &pxv1a1.ProxmoxMachine{}
	g.Expect(c.Get(ctx, key, pm)).To(Succeed())
	g.Expect(pm.Spec.Pool).To(Equal(pxv1a1.DefaultPool))
	g.Expect(pm.Finalizers).To(ContainElement(pxv1a1.Finalizer))

	// Power off through the portable object only.
	vm = getVM()
	vm.Spec.PowerState = kubevmv1a1.PowerStateOff
	g.Expect(c.Update(ctx, vm)).To(Succeed())
	g.Eventually(getVM, 60*time.Second, 250*time.Millisecond).Should(
		WithTransform(func(v *kubevmv1a1.VirtualMachine) kubevmv1a1.PowerState { return v.Status.PowerState },
			Equal(kubevmv1a1.PowerStateOff)))
	g.Expect(px.CallsWithPrefix("shutdown")).To(HaveLen(1), "TrySoft (the default) is a shutdown")

	// And back on.
	vm = getVM()
	vm.Spec.PowerState = kubevmv1a1.PowerStateOn
	g.Expect(c.Update(ctx, vm)).To(Succeed())
	g.Eventually(func() string {
		_ = c.Get(ctx, key, pm)
		if cnd := meta.FindStatusCondition(pm.Status.Conditions, pxv1a1.ConditionInfrastructureReady); cnd != nil {
			return cnd.Reason
		}
		return ""
	}, 30*time.Second, 250*time.Millisecond).Should(Equal(pxv1a1.ReasonRunning))

	// Deleting the portable object removes the Proxmox VM and both objects.
	g.Expect(c.Delete(ctx, getVM())).To(Succeed())
	g.Eventually(func() bool {
		_, exists := px.Get(100)
		return exists
	}, 60*time.Second, 250*time.Millisecond).Should(BeFalse(), "Proxmox VM not destroyed")
	g.Eventually(func() error { return c.Get(ctx, key, &pxv1a1.ProxmoxMachine{}) },
		30*time.Second, 250*time.Millisecond).ShouldNot(Succeed())
	g.Eventually(func() error { return c.Get(ctx, key, &kubevmv1a1.VirtualMachine{}) },
		30*time.Second, 250*time.Millisecond).ShouldNot(Succeed())
}

type testWriter struct{ t *testing.T }

func (w *testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
