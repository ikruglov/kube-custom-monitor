/*
Copyright 2017 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package slo

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/api/core/v1"
)

type containerMilestones struct {
	name string
	init bool // InitContainer

	restarts int32

	// when add fields here modify merge/validate functions
	started   bool
	startedAt time.Time

	finished   bool
	finishedAt time.Time

	pulling   bool
	pullingAt time.Time

	pulled         bool
	pulledAt       time.Time
	alreadyPresent bool
}

type podMilestones struct {
	name      string
	namespace string
	uid       string

	containers map[string]*containerMilestones

	// when add fields here modify merge/validate functions
	createdAt time.Time

	// flag that says if given data was already used to add a datapoint to the metric.
	accountedFor bool
}

func (pm *podMilestones) key() string {
	return getPodKey(pm.namespace, pm.name, pm.uid)
}

func (pm *podMilestones) merge(input *podMilestones) {
	if pm.namespace != input.namespace || pm.name != input.name || pm.uid != input.uid {
		return
	}

	if isZero(pm.createdAt) || pm.createdAt.After(input.createdAt) {
		pm.createdAt = input.createdAt
	}

	for name, container := range input.containers {
		if c, ok := pm.containers[name]; ok {
			c.merge(container)
		} else {
			pm.containers[name] = container
		}
	}
}

func (pm *podMilestones) mergeContainer(input *containerMilestones) {
	if ml, ok := pm.containers[input.name]; ok {
		ml.merge(input)
	} else {
		pm.containers[input.name] = input
	}
}

func (pm *podMilestones) validate() error {
	if isZero(pm.createdAt) {
		return errors.New("pod creation time is empty")
	}

	if len(pm.containers) == 0 {
		return errors.New("pod has no containers")
	}

	for name, container := range pm.containers {
		if err := container.validate(); err != nil {
			return fmt.Errorf("failed to validate container %q: %v", name, err)
		}
	}

	return nil
}

func (pm *podMilestones) allContainerStarted() bool {
	for _, container := range pm.containers {
		if !container.started {
			return false
		}
	}

	return len(pm.containers) > 0
}

func (pm *podMilestones) latestContainerStartedAt() time.Time {
	latest := time.Unix(0, 0)
	for _, container := range pm.containers {
		if container.started && container.startedAt.After(latest) {
			latest = container.startedAt
		}
	}

	return latest
}

func (pm *podMilestones) getStartupTime() (time.Duration, error) {
	latest := pm.latestContainerStartedAt()
	duration := latest.Sub(pm.createdAt)
	return duration, validateDuration(duration)
}

func (cm *containerMilestones) merge(input *containerMilestones) {
	if cm.name != input.name {
		return
	}

	if input.init {
		cm.init = true
	}

	if input.restarts > cm.restarts {
		cm.restarts = input.restarts
	}

	if input.started {
		cm.started = true
		if isZero(cm.startedAt) || cm.startedAt.After(input.startedAt) {
			cm.startedAt = input.startedAt
		}
	}

	if input.finished {
		cm.finished = true
		if isZero(cm.finishedAt) || cm.finishedAt.Before(input.finishedAt) {
			cm.finishedAt = input.finishedAt
		}
	}

	if input.pulling {
		cm.pulling = true
		if isZero(cm.pullingAt) || cm.pullingAt.After(input.pullingAt) {
			cm.pullingAt = input.pullingAt
		}
	}

	if input.pulled {
		cm.pulled = true
		cm.alreadyPresent = cm.alreadyPresent || input.alreadyPresent
		if isZero(cm.pulledAt) || cm.pulledAt.Before(input.pulledAt) {
			cm.pulledAt = input.pulledAt
		}
	}
}

func (cm *containerMilestones) validate() error {
	if !cm.started || isZero(cm.startedAt) {
		return errors.New("container never started")
	}

	if cm.init && (!cm.finished || isZero(cm.finishedAt)) {
		return errors.New("init container never finished")
	}

	if cm.restarts > 0 {
		return errors.New("this is not a first run of the container")
	}

	if !cm.pulled {
		return errors.New("container didn't finished pulling")
	}

	if cm.alreadyPresent {
		if isZero(cm.pulledAt) {
			return errors.New("container didn't finished pulling")
		}
	} else {
		if !cm.pulling || isZero(cm.pullingAt) {
			return errors.New("container didn't start pulling")
		}
	}

	return nil
}

func (cm *containerMilestones) getPullDuration() (time.Duration, error) {
	if cm.alreadyPresent {
		return 0, nil
	}

	duration := cm.pulledAt.Sub(cm.pullingAt)
	return duration, validateDuration(duration)
}

func (cm *containerMilestones) getRunningDuration() (time.Duration, error) {
	duration := cm.finishedAt.Sub(cm.startedAt)
	return duration, validateDuration(duration)
}

func newPodMilestonesFromReference(objRef v1.ObjectReference) *podMilestones {
	var result podMilestones
	result.name = objRef.Name
	result.namespace = objRef.Namespace
	result.uid = string(objRef.UID)
	result.containers = make(map[string]*containerMilestones)
	result.createdAt = time.Unix(0, 0) // necessary to work anywere except UTC time zone...
	return &result
}

func newPodMilestonesFromPod(pod *v1.Pod) *podMilestones {
	var result podMilestones
	result.name = pod.ObjectMeta.Name
	result.namespace = pod.ObjectMeta.Namespace
	result.uid = string(pod.ObjectMeta.UID)
	result.createdAt = pod.CreationTimestamp.Time
	result.containers = make(map[string]*containerMilestones)

	for _, status := range pod.Status.InitContainerStatuses {
		startedAt, finishedAt := getContainerTimings(status)

		ml := newContainerMilestones(status.Name)
		ml.restarts = status.RestartCount
		ml.init = true

		ml.started = !isZero(startedAt)
		ml.startedAt = startedAt

		ml.finished = !isZero(finishedAt)
		ml.finishedAt = finishedAt

		result.containers[status.Name] = ml
	}

	for _, status := range pod.Status.ContainerStatuses {
		startedAt, _ := getContainerTimings(status)

		ml := newContainerMilestones(status.Name)
		ml.restarts = status.RestartCount
		ml.started = !isZero(startedAt)
		ml.startedAt = startedAt

		result.containers[status.Name] = ml
	}

	return &result
}

func newContainerMilestones(name string) *containerMilestones {
	var result containerMilestones
	result.name = name
	result.startedAt = time.Unix(0, 0)  // necessary to work anywere except UTC time zone...
	result.finishedAt = time.Unix(0, 0) // necessary to work anywere except UTC time zone...
	result.pullingAt = time.Unix(0, 0)  // necessary to work anywere except UTC time zone...
	result.pulledAt = time.Unix(0, 0)   // necessary to work anywere except UTC time zone...
	return &result
}

func getContainerTimings(status v1.ContainerStatus) (time.Time, time.Time) {
	if terminated := status.State.Terminated; terminated != nil {
		// pod completed
		return terminated.StartedAt.Time, terminated.FinishedAt.Time
	}

	if running := status.State.Running; running != nil {
		// pod is currently running
		return running.StartedAt.Time, time.Unix(0, 0)
	}

	return time.Unix(0, 0), time.Unix(0, 0)
}

func validateDuration(duration time.Duration) error {
	if duration < 0 {
		return fmt.Errorf("computed negative duration: %s", duration)
	}

	if duration >= 24*time.Hour {
		return fmt.Errorf("computed duration is abnormaly big: %s", duration)
	}

	return nil
}

func (pm *podMilestones) String() string {
	var result []string
	result = append(result, fmt.Sprintf(
		"podMilestones: name: %q namespace: %q uid: %q createdAt: %s accountedFor: %t",
		pm.name, pm.namespace, pm.uid,
		formatAt(pm.createdAt),
		pm.accountedFor,
	))

	for _, container := range pm.containers {
		result = append(result, container.String())
	}

	return strings.Join(result, "\n")
}

func (cm *containerMilestones) String() string {
	return fmt.Sprintf(
		"containerMilestones: name: %q init: %t restarts: %d started: %t startedAt: %s finished: %t finishedAt: %s pulling: %t pullingAt: %s pulled: %t pulledAt: %s alreadyPresent: %t",
		cm.name, cm.init, cm.restarts,
		cm.started, formatAt(cm.startedAt),
		cm.finished, formatAt(cm.finishedAt),
		cm.pulling, formatAt(cm.pullingAt),
		cm.pulled, formatAt(cm.pulledAt), cm.alreadyPresent,
	)
}

func isZero(t time.Time) bool {
	return t.IsZero() || t.UnixNano() == 0
}

func formatAt(t time.Time) string {
	if isZero(t) {
		return ""
	}

	return t.String()
}
