package main

import (
	"flag"
	"net/http"
	"time"

	"github.com/golang/glog"
	"github.com/ikruglov/kube-custom-monitor/clusteriprange"
	"github.com/ikruglov/kube-custom-monitor/leader"
	"github.com/ikruglov/kube-custom-monitor/pod"
	"github.com/ikruglov/kube-custom-monitor/registry"
	"github.com/ikruglov/kube-custom-monitor/slo"
	"github.com/ikruglov/kube-custom-monitor/util"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	BuildVersion = "(development build)"
	monitors     map[string]registry.Monitor
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig file")
	bindAddress := flag.String("metrics-bind-address", "127.0.0.1", "Address to bind to")
	bindPort := flag.String("metrics-bind-port", "8080", "Port to bind to")
	resyncPeriod := flag.Duration("watcher-resync-period", 5*time.Minute, "interval at which the watchers should re-sync with k8s api server")
	flag.Parse()
	defer glog.Flush()

	glog.Infoln("Starting kube-monitor", BuildVersion)
	monitors := make(map[string]registry.Monitor)

	client, err := util.NewKubernetesClient(*kubeconfig, nil)
	if err != nil {
		glog.Fatal(err)
	}

	glog.Infof("initializing monitors")
	reg := registry.New(client, *resyncPeriod)

	if m, err := leader.NewMonitor(reg); err != nil {
		glog.Fatalf("failed to create leader monitor: %s", err)
	} else {
		monitors["leader"] = m
	}

	if m, err := slo.NewMonitor(reg); err != nil {
		glog.Fatalf("failed to create slo monitor: %s", err)
	} else {
		monitors["slo"] = m
	}

	if m, err := clusteriprange.NewMonitor(reg); err != nil {
		glog.Fatalf("failed to create clusteriprange monitor: %s", err)
	} else {
		monitors["clusteriprange"] = m
	}

	if m, err := pod.NewMonitor(reg); err != nil {
		glog.Fatalf("failed to create pod monitor: %s", err)
	} else {
		monitors["pod"] = m
	}

	glog.Infof("starting work")
	stop := util.SetupSignalHandler()
	go reg.InformerFactory.Start(stop)

	for name, monitor := range monitors {
		go func(name string, m registry.Monitor) {
			if err := m.Run(stop); err != nil {
				glog.Fatalf("failed to run monitor %q: %v", name, err)
			}
		}(name, monitor)
	}

	go func() {
		http.Handle("/metrics", promhttp.InstrumentMetricHandler(
			reg.PrometheusRegistry,
			promhttp.HandlerFor(reg.PrometheusRegistry, promhttp.HandlerOpts{}),
		))

		bind := *bindAddress + ":" + *bindPort
		glog.Infof("start metrics at %s", bind)
		if err := http.ListenAndServe(bind, nil); err != nil {
			glog.Fatal(err)
		}
	}()

	<-stop
	glog.Infof("Bye!")
}
