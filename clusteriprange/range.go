package clusteriprange

import (
	"flag"
	"fmt"
	"net"

	"github.com/golang/glog"
	"github.com/ikruglov/kube-custom-monitor/registry"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	serviceClusterIPRange     string
	serviceClusterIPRangeSize = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "kube_service_cluster_ip_range_size",
			Help: "A gauge to how many IPs are within service cluster IP range.",
		},
	)
)

func init() {
	flag.StringVar(&serviceClusterIPRange, "service-cluster-ip-range", "", "A CIDR notation IP range from which to assign service cluster IPs.")
}

type monitor struct {
}

func NewMonitor(registry *registry.Registry) (*monitor, error) {
	if serviceClusterIPRange == "" {
		return &monitor{}, nil
	}

	_, ipnet, err := net.ParseCIDR(serviceClusterIPRange)
	if err != nil {
		return nil, fmt.Errorf("failed to parse %q: %v", serviceClusterIPRange, err)
	}

	if ones, bits := ipnet.Mask.Size(); bits > ones {
		totalIPs := (2 << uint(bits-ones-1)) - 2
		glog.Infof("service cluster IP range %q has %d IPs", serviceClusterIPRange, totalIPs)
		serviceClusterIPRangeSize.Set(float64(totalIPs))
		registry.PrometheusRegistry.MustRegister(serviceClusterIPRangeSize)
	}

	return &monitor{}, nil
}

func (m *monitor) Run(stop <-chan struct{}) error {
	return nil
}
