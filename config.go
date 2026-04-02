package main

import (
	"context"
	"os"
	"strings"

	"github.com/projecteru2/core/log"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	commonk8s "github.com/cocoonstack/cocoon-common/k8s"
)

const defaultKubeconfigPath = "/etc/cocoon/kubeconfig"

func explicitKubeconfigPathFromEnv(kubeconfig string, stat func(string) error) string {
	if path := strings.TrimSpace(kubeconfig); path != "" {
		return path
	}
	if stat(defaultKubeconfigPath) == nil {
		return defaultKubeconfigPath
	}
	return ""
}

func explicitKubeconfigPath() string {
	return explicitKubeconfigPathFromEnv(os.Getenv("KUBECONFIG"), func(path string) error {
		_, err := os.Stat(path)
		return err
	})
}

func loadKubeConfig(ctx context.Context) (*rest.Config, error) {
	logger := log.WithFunc("loadKubeConfig")
	if kubeconfig := explicitKubeconfigPath(); kubeconfig != "" {
		logger.Infof(ctx, "using kubeconfig %s", kubeconfig)
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}

	logger.Info(ctx, "using cocoon-common kubeconfig fallback chain")
	return commonk8s.LoadConfig()
}
