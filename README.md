# kube-custom-monitor
kube-custom-monitor - a set of useful Prometheus metrics for a kubernetes cluster

### Leader

Kubernetes setup

This monitor outputs two Prometheus metrics `leader_monitor_is_leader` and
`leader_monitor_leader_transitions` which reflect an active leader for
`kube-scheduler`, `kube-controller-manager` and other kubernetes controllers
which uses the same [leader election
library](https://godoc.org/k8s.io/client-go/tools/leaderelection).

### SLO

SLO monitor compute a series of metrics which reflect time which a POD took to start:

* `slomonitor_pod_e2e_startup_latency_seconds` - POD end-to-end startup latency without image pull time
* `slomonitor_pod_image_pull_latency_seconds` - POD image pull latency
* `slomonitor_pod_full_startup_latency_seconds` - POD's end-to-end startup latency

A startup latency is computed as the time from POD's object creating time to
the start time of the last container in a pod minus time it took to download
the images and minus time it took to run init-containers.

This module is heavily inspired by
[slo-monitor](https://github.com/kubernetes/perf-tests/tree/master/slo-monitor).
However, they differently understand startup time. For slo-monitor, the startup
time includes init-containers and readiness probes. In a production
environment, these two values can be modified by cluster's users, and such
modifications can heavily skew the cluster's SLO.

### POD

One of the disadvantages of
[kube-state-metrics](https://github.com/kubernetes/kube-state-metrics) is that
the set of metrics it often reports hard to correlate with the POD's state
which `kubectl get pods` outputs.


```
$ kubectl get pods
NAME                             READY     STATUS    RESTARTS   AGE
api-f13f3c93-0-b469c776c-x8b25   1/1       Running   8          8d
```

This monitor reads the API server and computes a single metric `kube_pod_status`
which value matches the status of a POD as how `kubectl` would show it.

## Build and use

```
$ https://github.com/ikruglov/kube-custom-monitor.git
$ cd kube-custom-monitor
$ go build -v
$ ./kube-custom-monitor -kubeconfig kubeconfig -logtostderr -v 1

# in parallel terminal
$ curl -q http://localhost:8082/metrics
```

## Acknowledgement
* Idea and initial implementation is by @sparky
* This module was originally developed for Booking.com.
