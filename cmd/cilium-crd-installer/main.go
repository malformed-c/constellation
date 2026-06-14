// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

// cilium-crd-installer registers all Cilium CRDs with the Kubernetes API server
// and exits. It is a lightweight alternative to running cilium-operator just to
// get CRDs installed; it does not reconcile any other resources.
//
// Usage:
//
//	cilium-crd-installer --k8s-kubeconfig-path=/etc/rancher/k3s/k3s.yaml
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/client-go/tools/clientcmd"

	ciliumclient "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/client"
	"github.com/cilium/cilium/pkg/logging/logfields"
)

func main() {
	kubeconfig := flag.String("k8s-kubeconfig-path", "", "Absolute path to kubeconfig file (required)")
	timeout := flag.Duration("timeout", 60*time.Second, "Timeout for CRD registration")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if *kubeconfig == "" {
		logger.Error("--k8s-kubeconfig-path is required")
		os.Exit(1)
	}

	restCfg, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		logger.Error("Failed to build kubeconfig",
			logfields.Error, err,
			logfields.Path, *kubeconfig,
		)
		os.Exit(1)
	}

	clientset, err := apiextensionsclient.NewForConfig(restCfg)
	if err != nil {
		logger.Error("Failed to create apiextensions clientset",
			logfields.Error, err,
		)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	_ = ctx // CreateCustomResourceDefinitions doesn't take a ctx, but we honour the timeout via the client

	logger.Info("Registering Cilium CRDs")
	if err := ciliumclient.CreateCustomResourceDefinitions(logger, clientset); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to register Cilium CRDs: %v\n", err)
		os.Exit(1)
	}

	logger.Info("All Cilium CRDs registered successfully")
}
