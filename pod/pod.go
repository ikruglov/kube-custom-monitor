package pod

import (
	"fmt"

	"github.com/golang/glog"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	core_v1 "k8s.io/client-go/listers/core/v1"

	"github.com/ikruglov/kube-custom-monitor/registry"
)

var descPodStatus = prometheus.NewDesc(
	"kube_pod_status",
	"The pods current status as kubectl would report it.",
	[]string{"namespace", "pod", "status"}, nil,
)

type monitor struct {
	podLister core_v1.PodLister
}

func (m *monitor) Describe(ch chan<- *prometheus.Desc) {
	ch <- descPodStatus
}

func (m *monitor) Collect(ch chan<- prometheus.Metric) {
	pods, err := m.podLister.List(labels.Everything())
	if err != nil {
		glog.Errorf("failed to list pods: %v", err)
		return
	}

	for _, pod := range pods {
		status := getPodStatus(pod)
		lv := []string{pod.Namespace, pod.Name, status}
		ch <- prometheus.MustNewConstMetric(descPodStatus, prometheus.GaugeValue, 1, lv...)
	}
}

func NewMonitor(registry *registry.Registry) (*monitor, error) {
	m := &monitor{podLister: registry.InformerFactory.Core().V1().Pods().Lister()}
	registry.PrometheusRegistry.MustRegister(m)
	return m, nil
}

func (m *monitor) Run(stop <-chan struct{}) error {
	return nil
}

// copy-pasted from printPod() in pkg/printers/internalversion/printers.go
func getPodStatus(pod *v1.Pod) string {
	restarts := 0
	// totalContainers := len(pod.Spec.Containers)
	readyContainers := 0

	reason := string(pod.Status.Phase)
	if pod.Status.Reason != "" {
		reason = pod.Status.Reason
	}

	initializing := false
	for i := range pod.Status.InitContainerStatuses {
		container := pod.Status.InitContainerStatuses[i]
		restarts += int(container.RestartCount)
		switch {
		case container.State.Terminated != nil && container.State.Terminated.ExitCode == 0:
			continue
		case container.State.Terminated != nil:
			// initialization is failed
			if len(container.State.Terminated.Reason) == 0 {
				if container.State.Terminated.Signal != 0 {
					reason = fmt.Sprintf("Init:Signal:%d", container.State.Terminated.Signal)
				} else {
					reason = fmt.Sprintf("Init:ExitCode:%d", container.State.Terminated.ExitCode)
				}
			} else {
				reason = "Init:" + container.State.Terminated.Reason
			}
			initializing = true
		case container.State.Waiting != nil && len(container.State.Waiting.Reason) > 0 && container.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + container.State.Waiting.Reason
			initializing = true
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(pod.Spec.InitContainers))
			initializing = true
		}
		break
	}
	if !initializing {
		restarts = 0
		for i := len(pod.Status.ContainerStatuses) - 1; i >= 0; i-- {
			container := pod.Status.ContainerStatuses[i]

			restarts += int(container.RestartCount)
			if container.State.Waiting != nil && container.State.Waiting.Reason != "" {
				reason = container.State.Waiting.Reason
			} else if container.State.Terminated != nil && container.State.Terminated.Reason != "" {
				reason = container.State.Terminated.Reason
			} else if container.State.Terminated != nil && container.State.Terminated.Reason == "" {
				if container.State.Terminated.Signal != 0 {
					reason = fmt.Sprintf("Signal:%d", container.State.Terminated.Signal)
				} else {
					reason = fmt.Sprintf("ExitCode:%d", container.State.Terminated.ExitCode)
				}
			} else if container.Ready && container.State.Running != nil {
				readyContainers++
			}
		}
	}

	// have to copy-paste this const to avoid weird issues with redefining log_dir
	// taken from https://godoc.org/k8s.io/kubernetes/pkg/util/node
	const NodeUnreachablePodReason = "NodeLost"
	if pod.DeletionTimestamp != nil && pod.Status.Reason == NodeUnreachablePodReason {
		reason = "Unknown"
	} else if pod.DeletionTimestamp != nil {
		reason = "Terminating"
	}

	return reason
}
