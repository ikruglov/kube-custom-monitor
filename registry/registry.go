package registry

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	api_v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
)

type Registry struct {
	PrometheusRegistry *prometheus.Registry
	InformerFactory    informers.SharedInformerFactory
}

func New(clientset *kubernetes.Clientset, resyncPeriod time.Duration) *Registry {
	return &Registry{
		PrometheusRegistry: prometheus.NewRegistry(),
		InformerFactory:    informers.NewFilteredSharedInformerFactory(clientset, resyncPeriod, api_v1.NamespaceAll, nil),
	}
}

type Monitor interface {
	Run(stop <-chan struct{}) error
}
