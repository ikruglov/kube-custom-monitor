package leader

import (
	"encoding/json"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/ikruglov/kube-custom-monitor/registry"
	"github.com/prometheus/client_golang/prometheus"
	api_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	icore_v1 "k8s.io/client-go/informers/core/v1"
	lcore_v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

const namespace = "kube-system"

var (
	clockSkewToleration time.Duration

	isLeaderDesc = prometheus.NewDesc(
		"leader_monitor_is_leader",
		"A gauge to show who is a leader or not.",
		[]string{"name", "namespace", "holder_identity"},
		nil,
	)

	leaderTransitionsDesc = prometheus.NewDesc(
		"leader_monitor_leader_transitions",
		"A gauge to show who many time leader has changed.",
		[]string{"name", "namespace"},
		nil,
	)
)

func init() {
	flag.DurationVar(&clockSkewToleration, "leader-clock-skew-toleration", 30*time.Second, "An option to tolerate clock skew difference between cluster nodes")
}

type monitor struct {
	informer icore_v1.EndpointsInformer
	lister   lcore_v1.EndpointsNamespaceLister
	leaders  map[string]string
}

func (m *monitor) Describe(ch chan<- *prometheus.Desc) {
	ch <- isLeaderDesc
	ch <- leaderTransitionsDesc
}

func (m *monitor) Collect(ch chan<- prometheus.Metric) {
	if endpoints, err := m.lister.List(labels.Everything()); err == nil {
		for _, ep := range endpoints {
			if err := m.processEndpoints(ch, ep); err != nil {
				glog.Errorf("failed to process endpoint %q: %v", ep.ObjectMeta.Name, err)
			}
		}
	} else {
		glog.Errorf("failed list endpoints in namespace %q: %v", namespace, err)
	}
}

func NewMonitor(registry *registry.Registry) (*monitor, error) {
	m := &monitor{
		informer: registry.InformerFactory.Core().V1().Endpoints(),
		lister:   registry.InformerFactory.Core().V1().Endpoints().Lister().Endpoints(namespace),
		leaders:  make(map[string]string),
	}

	m.informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: func(obj interface{}) {}})
	registry.PrometheusRegistry.MustRegister(m)
	return m, nil
}

func (m *monitor) Run(stop <-chan struct{}) error {
	informer := m.informer.Informer()
	go informer.Run(stop)
	if !cache.WaitForCacheSync(stop, informer.HasSynced) {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	return nil
}

func (m *monitor) processEndpoints(ch chan<- prometheus.Metric, endpoints *api_v1.Endpoints) error {
	for k, v := range endpoints.ObjectMeta.Annotations {
		if strings.Contains(k, "leader") {
			return m.processLabel(ch, endpoints.ObjectMeta.Name, v)
		}
	}

	return nil
}

func (m *monitor) processLabel(ch chan<- prometheus.Metric, name, label string) error {
	glog.V(4).Infof("processing %q %s", name, label)

	// {
	// "renewTime" : "2018-03-15T20:46:43Z",
	// "holderIdentity" : "...",
	// "leaseDurationSeconds" : 15,
	// "acquireTime" : "2018-03-15T13:01:21Z",
	// "leaderTransitions" : 68
	// }

	var leaderInfo struct {
		RenewTime            string `json:"renewTime"`
		HolderIdentity       string `json:"holderIdentity"`
		LeaseDurationSeconds uint   `json:"leaseDurationSeconds"`
		// AcquireTime          string `json:"acquireTime"`
		LeaderTransitions uint `json:"leaderTransitions"`
	}

	if err := json.Unmarshal([]byte(label), &leaderInfo); err != nil {
		return fmt.Errorf("failed to decode leader info: %v", err)
	}

	renewTime, err := time.Parse(time.RFC3339, leaderInfo.RenewTime)
	if err != nil {
		return fmt.Errorf("failed to parse 'renewTime': %v", err)
	}

	leaseDuration := time.Duration(leaderInfo.LeaseDurationSeconds) * time.Second
	expireTime := renewTime.Add(leaseDuration)

	// so, since the endpoint metric is produces by a host which doesn't
	// neccessary the one where kube-monitor runs, there might be a clock skew
	// between them. This logic is here to tolerate a small amount of such skew.
	//
	// by adding and substracting values we simply extend the time frame in
	// which an entry is considered to be a leader

	now := time.Now()
	renewTimeLow := renewTime.Add(-clockSkewToleration)
	expireTimeHigh := expireTime.Add(clockSkewToleration)
	isLeader := (renewTimeLow.Before(now) || renewTimeLow.Equal(now)) && (expireTimeHigh.After(now) || expireTimeHigh.Equal(now))

	glog.V(4).Infof(
		"result %q: leader %t renewTime %q expireTime %q renewTimeLow %q expireTimeHigh %q now %q",
		name, isLeader, renewTime, expireTime, renewTimeLow, expireTimeHigh, now,
	)

	if isLeader {
		ch <- prometheus.MustNewConstMetric(isLeaderDesc, prometheus.GaugeValue, 1, name, namespace, leaderInfo.HolderIdentity)
		ch <- prometheus.MustNewConstMetric(leaderTransitionsDesc, prometheus.GaugeValue, float64(leaderInfo.LeaderTransitions), name, namespace)

		newLeader := leaderInfo.HolderIdentity
		oldLeader := m.leaders[name]
		m.leaders[name] = newLeader

		if oldLeader != newLeader {
			glog.Infof("%q in %q got new leader %q", name, namespace, newLeader)
		}
	}

	return nil
}
