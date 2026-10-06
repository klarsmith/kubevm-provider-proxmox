// SPDX-License-Identifier: Apache-2.0

// Command manager runs the Proxmox VE provider for KubeVM, with the KubeVM
// core controller hosted in the same process.
package main

import (
	"flag"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kubevmv1a1 "github.com/vmware-tanzu/vm-operator/external/kubevm/api/v1alpha1"

	pxv1a1 "github.com/klarsmith/kubevm-provider-proxmox/api/v1alpha1"
	"github.com/klarsmith/kubevm-provider-proxmox/controllers"
)

var scheme = runtime.NewScheme()

// Both API groups: this provider's and kube-vm.io's. Missing the second
// fails at runtime, on the first Get of a VirtualMachine.
func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(pxv1a1.AddToScheme(scheme))
	utilruntime.Must(kubevmv1a1.AddToScheme(scheme))
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var metricsAddr, probeAddr, allowedPool string
	var leaderElect bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0",
		`The address the metrics endpoint binds to. "0" disables it.`)
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.StringVar(&allowedPool, "allowed-pool", pxv1a1.DefaultPool,
		"The only Proxmox pool this controller creates, changes or deletes VMs in.")
	flag.BoolVar(&leaderElect, "leader-elect", false,
		"Enable leader election, for running more than one replica.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("kubevm-provider-proxmox")

	if allowedPool == "" {
		return fmt.Errorf("--allowed-pool must not be empty")
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		// Credentials Secrets are read directly, not through the cache. A
		// cached read would start a cluster-wide Secret informer, which
		// needs list/watch on all Secrets; the RBAC grants only get. With
		// the cache, the first Get blocks forever on an informer that can
		// never sync.
		Client:           client.Options{Cache: &client.CacheOptions{DisableFor: []client.Object{&corev1.Secret{}}}},
		Metrics:          metricsserver.Options{BindAddress: metricsAddr},
		LeaderElection:   leaderElect,
		LeaderElectionID: "kubevm-provider-proxmox.infrastructure.kube-vm.io",
	})
	if err != nil {
		return fmt.Errorf("building the manager: %w", err)
	}

	if err := controllers.AddToManager(mgr, controllers.Options{AllowedPool: allowedPool}); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("adding the health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("adding the readiness check: %w", err)
	}

	log.Info("starting manager", "allowedPool", allowedPool)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("running the manager: %w", err)
	}
	return nil
}
