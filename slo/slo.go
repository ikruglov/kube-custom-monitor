package slo

import (
	"flag"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/ikruglov/kube-custom-monitor/registry"
	"github.com/prometheus/client_golang/prometheus"
	api_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	core_v1 "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"
)

const (
	// source https://pkg.go.dev/k8s.io/kubernetes/pkg/kubelet/events
	PullingImage            = "Pulling"
	PulledImage             = "Pulled"
	FailedToPullImage       = "Failed"
	FailedToInspectImage    = "InspectFailed"
	ErrImageNeverPullPolicy = "ErrImageNeverPull"
	BackOffPullImage        = "BackOff"
)

var (
	// podE2EStartupLatency is a prometheus metric for monitoring pod startup latency.
	podE2EStartupLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "slomonitor_pod_e2e_startup_latency_seconds",
			Help:    "Pod e2e startup latencies in seconds, without image pull times",
			Buckets: prometheus.ExponentialBuckets(0.5, 1.5, 15),
		},
	)

	// podImagePullLatency is a prometheus metric for monitoring pod image pull latency
	podImagePullLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "slomonitor_pod_image_pull_latency_seconds",
			Help:    "Pod image pull latencies in seconds",
			Buckets: prometheus.ExponentialBuckets(0.5, 1.5, 15),
		},
	)

	// podFullStartupLatency is a prometheus metric for monitoring pod startup latency including image pull times.
	podFullStartupLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "slomonitor_pod_full_startup_latency_seconds",
			Help:    "Pod e2e startup latencies in seconds, with image pull times",
			Buckets: prometheus.ExponentialBuckets(0.5, 1.5, 15),
		},
	)

	purgeAfter            time.Duration
	includeInitContainers string
)

func init() {
	flag.DurationVar(&purgeAfter, "slo-purge-after-seconds", 120*time.Second, "Time after which deleted entries are purged.")
	flag.StringVar(&includeInitContainers, "slo-include-init-containers", "", "Comma separated list of names of init-containers to include into pod startup (all init containers are excluded by default")
}

type podStartupLatencyDataMonitor struct {
	podInformer   core_v1.PodInformer
	eventInformer core_v1.EventInformer

	inits map[string]bool
	pods  map[string]*podMilestones

	// Map of pods marked for deletion waiting to be purged. Just an
	// optimization to avoid full scans.
	toDelete map[string]time.Time

	// Time after which deleted entries are purged. We don't want to delete
	// data right after observing deletions, because we may get events
	// corresponding to the given Pod later, which would result in recreating
	// entry, and a serious memory leak.
	purgeAfter time.Duration

	initDone bool
	sync.Mutex
}

func NewMonitor(registry *registry.Registry) (*podStartupLatencyDataMonitor, error) {
	registry.PrometheusRegistry.MustRegister(podE2EStartupLatency)
	registry.PrometheusRegistry.MustRegister(podImagePullLatency)
	registry.PrometheusRegistry.MustRegister(podFullStartupLatency)

	inits := make(map[string]bool)
	for _, name := range strings.Split(includeInitContainers, ",") {
		if n := strings.TrimSpace(name); n != "" {
			inits[n] = true
		}
	}

	glog.Infof("starting SLO monitor: initContainers: %v purgeAfter: %s", inits, purgeAfter)

	return &podStartupLatencyDataMonitor{
		podInformer:   registry.InformerFactory.Core().V1().Pods(),
		eventInformer: registry.InformerFactory.Core().V1().Events(),
		inits:         inits,
		pods:          make(map[string]*podMilestones),
		toDelete:      make(map[string]time.Time),
		purgeAfter:    purgeAfter,
	}, nil
}

func (pm *podStartupLatencyDataMonitor) Run(stop <-chan struct{}) error {
	eventInformer := pm.eventInformer.Informer()
	eventInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    func(obj interface{}) { pm.handleEvent(obj.(*api_v1.Event)) },
			UpdateFunc: func(old, new interface{}) { pm.handleEvent(new.(*api_v1.Event)) },
		},
		0*time.Second, // disable resync
	)

	podInformer := pm.podInformer.Informer()
	podInformer.AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				pm.handlePodUpdate(obj.(*api_v1.Pod))
			},
			UpdateFunc: func(old, new interface{}) {
				pm.handlePodUpdate(new.(*api_v1.Pod))
			},
			DeleteFunc: func(obj interface{}) {
				pod, ok := obj.(*api_v1.Pod)
				if !ok {
					if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
						if pod, ok = d.Obj.(*api_v1.Pod); !ok {
							glog.Errorf("failed to cast embedded object from tombstone to *v1.Pod: %v", d.Obj)
							return
						}
					} else {
						glog.Errorf("failed to cast observed object to *v1.Pod: %v", obj)
						return
					}
				}

				pm.handlePodDelete(pod)
			},
		},
		0*time.Second, // disable resync
	)

	go eventInformer.Run(stop)
	go podInformer.Run(stop)
	if !cache.WaitForCacheSync(stop, podInformer.HasSynced, eventInformer.HasSynced) {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	pm.initDone = true

	go func() {
		wait.Until(func() {
			pm.Lock()
			defer pm.Unlock()

			now := time.Now()
			for podKey, when := range pm.toDelete {
				if when.Before(now) {
					glog.V(1).Infof("cleanup pod %s", podKey)
					delete(pm.toDelete, podKey)
					delete(pm.pods, podKey)
				}
			}
		}, 15*time.Second, stop)
	}()

	return nil
}

func (pm *podStartupLatencyDataMonitor) handleEvent(event *api_v1.Event) {
	if !pm.initDone {
		return
	}

	switch event.Reason {
	case PullingImage:
		go func() {
			if err := pm.handlePullingImageEvent(event); err != nil {
				glog.Warningf("failed to process 'PullingImage' event: %v", err)
			}
		}()
	case PulledImage:
		go func() {
			if err := pm.handlePulledImageEvent(event); err != nil {
				glog.Warningf("failed to process 'PulledImage' event: %v", err)
			}
		}()
	}
}

func (pm *podStartupLatencyDataMonitor) handlePullingImageEvent(event *api_v1.Event) error {
	containerName, err := fetchContainerName(event.InvolvedObject)
	if err != nil {
		return fmt.Errorf("failed to fetch container name: %v", err)
	}

	containerMilestones := newContainerMilestones(containerName)
	containerMilestones.pullingAt = event.FirstTimestamp.Time
	containerMilestones.pulling = true

	pm.Lock()
	defer pm.Unlock()

	podKey := getPodKeyFromReference(event.InvolvedObject)
	podMilestones, ok := pm.pods[podKey]
	if !ok {
		podMilestones = newPodMilestonesFromReference(event.InvolvedObject)
		pm.pods[podKey] = podMilestones
	}

	podMilestones.mergeContainer(containerMilestones)
	glog.V(4).Infof("handlePullingImageEvent %q: %s", podKey, podMilestones)

	return pm.updateMetric(podMilestones)
}

func (pm *podStartupLatencyDataMonitor) handlePulledImageEvent(event *api_v1.Event) error {
	containerName, err := fetchContainerName(event.InvolvedObject)
	if err != nil {
		return fmt.Errorf("failed to fetch container name: %v", err)
	}

	containerMilestones := newContainerMilestones(containerName)
	containerMilestones.alreadyPresent = strings.Contains(event.Message, "already present on machine")
	containerMilestones.pulledAt = event.FirstTimestamp.Time
	containerMilestones.pulled = true

	pm.Lock()
	defer pm.Unlock()

	podKey := getPodKeyFromReference(event.InvolvedObject)
	podMilestones, ok := pm.pods[podKey]
	if !ok {
		podMilestones = newPodMilestonesFromReference(event.InvolvedObject)
		pm.pods[podKey] = podMilestones
	}

	podMilestones.mergeContainer(containerMilestones)
	glog.V(4).Infof("handlePulledImageEvent %q: %s", podKey, podMilestones)

	return pm.updateMetric(podMilestones)
}

func (pm *podStartupLatencyDataMonitor) handlePodUpdate(pod *api_v1.Pod) {
	if !pm.initDone {
		return
	}

	if pml := newPodMilestonesFromPod(pod); pml.allContainerStarted() {
		go func() {
			if err := pm.podUpdate(pml); err != nil {
				glog.Warningf("failed to process pod update %q: %v", pml.key(), err)
			}
		}()
	}
}

func (pm *podStartupLatencyDataMonitor) podUpdate(pml *podMilestones) error {
	pm.Lock()
	defer pm.Unlock()

	podKey := pml.key()
	if ml, ok := pm.pods[podKey]; ok {
		ml.merge(pml)
		glog.V(4).Infof("podUpdate exists %q: %s", podKey, ml)
	} else {
		pm.pods[podKey] = pml
		glog.V(4).Infof("podUpdate new %q: %s", podKey, pml)
	}

	return pm.updateMetric(pm.pods[podKey])
}

func (pm *podStartupLatencyDataMonitor) handlePodDelete(pod *api_v1.Pod) {
	pm.Lock()
	defer pm.Unlock()
	at := time.Now().Add(pm.purgeAfter)
	podKey := getPodKey(pod.ObjectMeta.Namespace, pod.ObjectMeta.Name, string(pod.ObjectMeta.UID))
	glog.V(4).Infof("schedule pod %q to cleanup at %s", podKey, at)
	pm.toDelete[podKey] = at
}

func (pm *podStartupLatencyDataMonitor) updateMetric(pml *podMilestones) error {
	key := pml.key()
	if err := pml.validate(); err != nil {
		glog.V(2).Infof("can't update metric for pod %q: %v", key, err)
		return nil
	}

	if pml.accountedFor {
		glog.V(2).Infof("metric for pod %q already updated", key)
		return nil
	}

	pml.accountedFor = true

	if !pm.initDone {
		glog.V(4).Infof("ignore metric for pod %q because of initial listing", key)
		return nil
	}

	glog.V(1).Infof("observed pod %q 1: created %s latestContainerStartedAt %s",
		key, pml.createdAt, pml.latestContainerStartedAt())
	glog.V(4).Infof("%s", pml)

	// full pod startup time, i.e. from creating time until the latest
	// container got started including image pulling
	fullStartupTime, err := pml.getStartupTime()
	if err != nil {
		return fmt.Errorf("got invalid full startup time (including image pull) for %q: %v", key, err)
	}

	var fullPullImageTime time.Duration                 // cumulative pull image time
	var fullInitContainersRunningTime time.Duration     // cumulative time of running all init containers
	var approvedInitContainersRunningTime time.Duration // time to run init container passed via --slo-include-init-containers

	for name, container := range pml.containers {
		if duration, err := container.getPullDuration(); err == nil {
			fullPullImageTime += duration
		} else {
			return fmt.Errorf("got invalid image pull time for container %q in pod %q: %v", name, key, err)
		}

		if container.init {
			if duration, err := container.getRunningDuration(); err == nil {
				fullInitContainersRunningTime += duration

				if pm.inits[name] {
					approvedInitContainersRunningTime += duration
				}
			} else {
				return fmt.Errorf("got invalid running time for init container %q in pod %q: %v", name, key, err)
			}
		}
	}

	startupTime := fullStartupTime - fullPullImageTime - fullInitContainersRunningTime + approvedInitContainersRunningTime
	glog.V(1).Infof("observed pod %q 2: podE2EStartupLatency: %s, fullStartupTime: %s fullPullImageTime: %s",
		key, startupTime, fullStartupTime, fullPullImageTime)
	glog.V(1).Infof("observed pod %q 3: fullInitContainersRunningTime: %s approvedInitContainersRunningTime: %s",
		key, fullInitContainersRunningTime, approvedInitContainersRunningTime)

	if err := validateDuration(startupTime); err != nil {
		return fmt.Errorf("failed to validate startupTime: %v", err)
	}

	if err := validateDuration(fullStartupTime); err != nil {
		return fmt.Errorf("failed to validate fullStartupTime: %v", err)
	}

	if err := validateDuration(fullPullImageTime); err != nil {
		return fmt.Errorf("failed to validate fullPullImageTime: %v", err)
	}

	podE2EStartupLatency.Observe(startupTime.Seconds())
	podFullStartupLatency.Observe(fullStartupTime.Seconds())
	podImagePullLatency.Observe(fullPullImageTime.Seconds())
	return nil
}
