package util

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type TweakConfigFunc func(*rest.Config)

func NewKubernetesClient(kubeconfig string, tweak TweakConfigFunc) (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	// Assuming the client is running inside a pod
	if kubeconfig == "" {
		config, err = rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to build in-cluster config: %v", err)
		}
	} else {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("failed to build out of the cluster config: %v", err)
		}
	}

	// this function will make all the modifications to the config object that the
	// user asks for
	if tweak != nil {
		tweak(config)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to kubernetes: %v", err)
	}

	return client, err
}
