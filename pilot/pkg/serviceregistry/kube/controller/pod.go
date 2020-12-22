// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"fmt"
	"sync"

	v1 "k8s.io/api/core/v1"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/tools/cache"

	"istio.io/istio/pilot/pkg/model"
	"istio.io/istio/pilot/pkg/serviceregistry/kube"
	"istio.io/istio/pilot/pkg/util/sets"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PodCache is an eventually consistent pod cache
type PodCache struct {
	informer cache.SharedIndexInformer

	sync.RWMutex
	// podsByIP maintains stable pod IP to name key mapping
	// this allows us to retrieve the latest status by pod IP.
	// This should only contain RUNNING or PENDING pods with an allocated IP.
	podsByIP map[string]string
	// IPByPods is a reverse map of podsByIP. This exists to allow us to prune stale entries in the
	// pod cache if a pod changes IP.
	IPByPods map[string]string

	podTransitionTime map[string]metav1.Time

	// needResync is map of IP to endpoint names. This is used to requeue endpoint
	// events when pod event comes. This typically happens when pod is not available
	// in podCache when endpoint event comes.
	needResync         map[string]sets.Set
	queueEndpointEvent func(string)

	c *Controller
}

func newPodCache(c *Controller, informer coreinformers.PodInformer, queueEndpointEvent func(string)) *PodCache {
	out := &PodCache{
		informer:           informer.Informer(),
		c:                  c,
		podsByIP:           make(map[string]string),
		IPByPods:           make(map[string]string),
		needResync:         make(map[string]sets.Set),
		queueEndpointEvent: queueEndpointEvent,
		podTransitionTime:  make(map[string]metav1.Time),
	}

	return out
}

// copy from kubernetes/pkg/api/v1/pod/utils.go
func IsPodReady(pod *v1.Pod) bool {
	return IsPodReadyConditionTrue(pod.Status)
}

// IsPodReadyConditionTrue returns true if a pod is ready; false otherwise.
func IsPodReadyConditionTrue(status v1.PodStatus) bool {
	condition := GetPodReadyCondition(status)
	return condition != nil && condition.Status == v1.ConditionTrue
}
func GetPodReadyCondition(status v1.PodStatus) *v1.PodCondition {
	_, condition := GetPodCondition(&status, v1.PodReady)
	return condition
}

func GetPodCondition(status *v1.PodStatus, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if status == nil {
		return -1, nil
	}
	return GetPodConditionFromList(status.Conditions, conditionType)
}

// GetPodConditionFromList extracts the provided condition from the given list of condition and
// returns the index of the condition and the condition. Returns -1 and nil if the condition is not present.
func GetPodConditionFromList(conditions []v1.PodCondition, conditionType v1.PodConditionType) (int, *v1.PodCondition) {
	if conditions == nil {
		return -1, nil
	}
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return i, &conditions[i]
		}
	}
	return -1, nil
}

// onEvent updates the IP-based index (pc.podsByIP).
func (pc *PodCache) onEvent(curr interface{}, ev model.Event) error {
	pc.Lock()
	defer pc.Unlock()

	// When a pod is deleted obj could be an *v1.Pod or a DeletionFinalStateUnknown marker item.
	pod, ok := curr.(*v1.Pod)
	if !ok {
		tombstone, ok := curr.(cache.DeletedFinalStateUnknown)
		if !ok {
			return fmt.Errorf("couldn't get object from tombstone %+v", curr)
		}
		pod, ok = tombstone.Obj.(*v1.Pod)
		if !ok {
			return fmt.Errorf("tombstone contained object that is not a pod %#v", curr)
		}
	}

	// This variable is used to determine whether we need to trigger an eds update for its dependencies
	// for example, when a running pod becomes unready, we should trigger an eds update to remove it from sidecar
	inTransitState := false
	ip := pod.Status.PodIP

	// PodIP will be empty when pod is just created, but before the IP is assigned
	// via UpdateStatus.
	if len(ip) > 0 {
		key := kube.KeyFunc(pod.Name, pod.Namespace)
		podCondition := GetPodReadyCondition(pod.Status)
		switch ev {
		case model.EventAdd:
			switch pod.Status.Phase {
			case v1.PodPending, v1.PodRunning:
				if key != pc.podsByIP[ip] {
					// add to cache if the pod is running or pending
					pc.update(ip, key)
				}
				if _, ok := pc.podTransitionTime[ip]; !ok && IsPodReady(pod) {
					pc.podTransitionTime[ip] = podCondition.LastTransitionTime
					inTransitState = true
				}
			}
		case model.EventUpdate:
			if pod.DeletionTimestamp != nil {
				// delete only if this pod was in the cache
				if pc.podsByIP[ip] == key {
					pc.deleteIP(ip)
				}
				delete(pc.podTransitionTime, ip)
				ev = model.EventDelete
			} else {
				switch pod.Status.Phase {
				case v1.PodPending, v1.PodRunning:
					if key != pc.podsByIP[ip] {
						// add to cache if the pod is running or pending
						pc.update(ip, key)
					}
					if IsPodReady(pod) {
						if lastTransitTime, ok := pc.podTransitionTime[ip]; !ok || !lastTransitTime.Equal(&podCondition.LastTransitionTime) {
							pc.podTransitionTime[ip] = podCondition.LastTransitionTime
							inTransitState = true
						}
					} else {
						// This is when a healthy endpoint becomes unready
						if _, ok := pc.podTransitionTime[ip]; ok {
							inTransitState = true
							pc.podTransitionTime[ip] = podCondition.LastTransitionTime
							ev = model.EventDelete
						}
					}
				default:
					// delete if the pod switched to other states and is in the cache
					if pc.podsByIP[ip] == key {
						pc.deleteIP(ip)
					}
					delete(pc.podTransitionTime, ip)
				}
			}
		case model.EventDelete:
			// delete only if this pod was in the cache
			if pc.podsByIP[ip] == key {
				pc.deleteIP(ip)
			}
			delete(pc.podTransitionTime, ip)
		}
		// fire instance handles for workload
		for _, handler := range pc.c.workloadHandlers {
			ep := NewEndpointBuilder(pc.c, pod).buildIstioEndpoint(ip, 0, "")
			handler(&model.WorkloadInstance{
				Name:           pod.Name,
				Namespace:      pod.Namespace,
				Endpoint:       ep,
				PortMap:        getPortMap(pod),
				InTransitState: inTransitState,
			}, ev)
		}
	}
	return nil
}

func getPortMap(pod *v1.Pod) map[string]uint32 {
	pmap := map[string]uint32{}
	for _, c := range pod.Spec.Containers {
		for _, port := range c.Ports {
			if port.Name == "" || port.Protocol != v1.ProtocolTCP {
				continue
			}
			// First port wins, per Kubernetes (https://github.com/kubernetes/kubernetes/issues/54213)
			if _, f := pmap[port.Name]; !f {
				pmap[port.Name] = uint32(port.ContainerPort)
			}
		}
	}
	return pmap
}

func (pc *PodCache) deleteIP(ip string) {
	pod := pc.podsByIP[ip]
	delete(pc.podsByIP, ip)
	delete(pc.IPByPods, pod)
}

func (pc *PodCache) update(ip, key string) {
	if current, f := pc.IPByPods[key]; f {
		// The pod already exists, but with another IP Address. We need to clean up that
		delete(pc.podsByIP, current)
	}
	pc.podsByIP[ip] = key
	pc.IPByPods[key] = ip

	if endpointsToUpdate, f := pc.needResync[ip]; f {
		delete(pc.needResync, ip)
		for epKey := range endpointsToUpdate {
			pc.queueEndpointEvent(epKey)
		}
		endpointsPendingPodUpdate.Record(float64(len(pc.needResync)))
	}

	pc.proxyUpdates(ip)
}

// queueEndpointEventOnPodArrival registers this endpoint and queues endpoint event
// when the corresponding pod arrives.
func (pc *PodCache) queueEndpointEventOnPodArrival(key, ip string) {
	pc.Lock()
	defer pc.Unlock()
	if _, f := pc.needResync[ip]; !f {
		pc.needResync[ip] = sets.NewSet(key)
	} else {
		pc.needResync[ip].Insert(key)
	}
	endpointsPendingPodUpdate.Record(float64(len(pc.needResync)))
}

// endpointDeleted cleans up endpoint from resync endpoint list.
func (pc *PodCache) endpointDeleted(key string, ip string) {
	pc.Lock()
	defer pc.Unlock()
	delete(pc.needResync[ip], key)
	if len(pc.needResync[ip]) == 0 {
		delete(pc.needResync, ip)
	}
	endpointsPendingPodUpdate.Record(float64(len(pc.needResync)))
}

func (pc *PodCache) proxyUpdates(ip string) {
	if pc.c != nil && pc.c.xdsUpdater != nil {
		pc.c.xdsUpdater.ProxyUpdate(pc.c.clusterID, ip)
	}
}

// nolint: unparam
func (pc *PodCache) getPodKey(addr string) (string, bool) {
	pc.RLock()
	defer pc.RUnlock()
	key, exists := pc.podsByIP[addr]
	return key, exists
}

// getPodByIp returns the pod or nil if pod not found or an error occurred
func (pc *PodCache) getPodByIP(addr string) *v1.Pod {
	key, exists := pc.getPodKey(addr)
	if !exists {
		return nil
	}
	return pc.getPodByKey(key)
}

// getPodByKey returns the pod by key formatted `ns/name`
func (pc *PodCache) getPodByKey(key string) *v1.Pod {
	item, _, _ := pc.informer.GetStore().GetByKey(key)
	if item != nil {
		return item.(*v1.Pod)
	}
	return nil
}

// getPodByKey returns the pod of the proxy
func (pc *PodCache) getPodByProxy(proxy *model.Proxy) *v1.Pod {
	var pod *v1.Pod
	key := podKeyByProxy(proxy)
	if key != "" {
		pod = pc.getPodByKey(key)
		if pod != nil {
			return pod
		}
	}

	// only need to fetch the corresponding pod through the first IP, although there are multiple IP scenarios,
	// because multiple ips belong to the same pod
	proxyIP := proxy.IPAddresses[0]
	// just in case the proxy ID is bad formatted
	return pc.getPodByIP(proxyIP)
}
