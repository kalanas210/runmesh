package k8s

import (
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// NewClient builds a clientset, preferring the in-cluster configuration.
//
// The order is deliberate. A pod running inside the cluster has a projected
// ServiceAccount token and should use it; falling back to a kubeconfig that
// happened to be baked into an image is how a workload ends up talking to a
// developer's laptop cluster from production. So in-cluster wins whenever it
// is available, and kubeconfig is the development path.
//
// An explicit kubeconfig path overrides both, because "point this at that
// cluster" has to be possible without argument.
func NewClient(kubeconfig, contextName string) (*kubernetes.Clientset, error) {
	cfg, err := restConfig(kubeconfig, contextName)
	if err != nil {
		return nil, err
	}

	// Defaults are 5 and 10, which throttles client-side long before the API
	// server would. One Job per attempt across a pool of workers is bursty by
	// nature, and a throttled create looks exactly like a slow cluster.
	cfg.QPS = 50
	cfg.Burst = 100
	cfg.UserAgent = "runmesh"

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: building the Kubernetes client: %w", err)
	}
	return client, nil
}

func restConfig(kubeconfig, contextName string) (*rest.Config, error) {
	if kubeconfig == "" && contextName == "" {
		if cfg, err := rest.InClusterConfig(); err == nil {
			return cfg, nil
		} else if err != rest.ErrNotInCluster {
			return nil, fmt.Errorf("k8s: reading the in-cluster configuration: %w", err)
		}
	}

	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	} else if rules.GetDefaultFilename() == "" {
		if home, err := os.UserHomeDir(); err == nil {
			rules.ExplicitPath = filepath.Join(home, ".kube", "config")
		}
	}

	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}

	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s: no usable Kubernetes configuration "+
			"(not running in a cluster, and kubeconfig did not load): %w", err)
	}
	return cfg, nil
}
