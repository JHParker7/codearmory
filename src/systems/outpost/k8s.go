package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// errNoModules is returned when no recognised module is enabled.
var errNoModules = errors.New("no recognised modules enabled")

// clientOrNil builds the dynamic client when any k8s-backed module is enabled,
// exiting on failure (a misconfigured outpost should fail loudly, not run
// half-initialised). Returns nil when no module needs cluster access.
func clientOrNil(needsK8s bool) dynamic.Interface {
	if !needsK8s {
		return nil
	}
	c, err := newDynamicClient()
	if err != nil {
		slog.Error("outpost: failed to build kubernetes client", "error", err)
		os.Exit(1)
	}
	return c
}

// newDynamicClient builds a client-go dynamic client, preferring in-cluster
// config and falling back to KUBECONFIG (or ~/.kube/config). Modeled on
// forge/runtime_k8s.go's config loading. The dynamic client lets modules work
// with arbitrary CRDs (litmuschaos.io, argoproj.io) as unstructured objects
// without compiled-in types.
func newDynamicClient() (dynamic.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			kubeconfig = clientcmd.RecommendedHomeFile
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("load kubeconfig: %w", err)
		}
	}
	client, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	return client, nil
}
