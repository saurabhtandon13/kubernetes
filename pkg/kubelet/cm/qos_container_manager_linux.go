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

package cm

import (
	"fmt"
	"path"
	"sync"
	"time"

	"github.com/golang/glog"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/api/v1"
	"k8s.io/kubernetes/pkg/kubelet/qos"
)

const (
	// how often the qos cgroup manager will perform periodic update
	// of the qos level cgroup resource constraints
	periodicQOSCgroupUpdateInterval = 1 * time.Minute
)

type QOSContainerManager interface {
	Start(func() v1.ResourceList, ActivePodsFunc) error
	GetQOSContainersInfo() QOSContainersInfo
	UpdateCgroups() error
}

type qosContainerManagerImpl struct {
	sync.Mutex
	nodeInfo           *v1.Node
	qosContainersInfo  QOSContainersInfo
	subsystems         *CgroupSubsystems
	cgroupManager      CgroupManager
	activePods         ActivePodsFunc
	getNodeAllocatable func() v1.ResourceList
	cgroupRoot         string
	qosReserved        map[v1.ResourceName]int64
}

func NewQOSContainerManager(subsystems *CgroupSubsystems, cgroupRoot string, nodeConfig NodeConfig) (QOSContainerManager, error) {
	if !nodeConfig.CgroupsPerQOS {
		return &qosContainerManagerNoop{
			cgroupRoot: CgroupName(nodeConfig.CgroupRoot),
		}, nil
	}

	return &qosContainerManagerImpl{
		subsystems:    subsystems,
		cgroupManager: NewCgroupManager(subsystems, nodeConfig.CgroupDriver),
		cgroupRoot:    cgroupRoot,
		qosReserved:   nodeConfig.ExperimentalQOSReserved,
	}, nil
}

func (m *qosContainerManagerImpl) GetQOSContainersInfo() QOSContainersInfo {
	return m.qosContainersInfo
}

func (m *qosContainerManagerImpl) Start(getNodeAllocatable func() v1.ResourceList, activePods ActivePodsFunc) error {
	cm := m.cgroupManager
	rootContainer := m.cgroupRoot
	if !cm.Exists(CgroupName(rootContainer)) {
		return fmt.Errorf("root container %s doesn't exist", rootContainer)
	}

	// Top level for Qos containers are created only for Burstable
	// and Best Effort classes
	qosClasses := [2]v1.PodQOSClass{v1.PodQOSBurstable, v1.PodQOSBestEffort}

	// Create containers for both qos classes
	for _, qosClass := range qosClasses {
		// get the container's absolute name
		absoluteContainerName := CgroupName(path.Join(rootContainer, string(qosClass)))

		resourceParameters := &ResourceConfig{}
		// the BestEffort QoS class has a statically configured minShares value
		if qosClass == v1.PodQOSBestEffort {
			minShares := int64(MinShares)
			resourceParameters.CpuShares = &minShares
		}
		// containerConfig object stores the cgroup specifications
		containerConfig := &CgroupConfig{
			Name:               absoluteContainerName,
			ResourceParameters: resourceParameters,
		}
		// check if it exists
		if !cm.Exists(absoluteContainerName) {
			if err := cm.Create(containerConfig); err != nil {
				return fmt.Errorf("failed to create top level %v QOS cgroup : %v", qosClass, err)
			}
		} else {
			// to ensure we actually have the right state, we update the config on startup
			if err := cm.Update(containerConfig); err != nil {
				return fmt.Errorf("failed to update top level %v QOS cgroup : %v", qosClass, err)
			}
		}
	}
	// Store the top level qos container names
	m.qosContainersInfo = QOSContainersInfo{
		Guaranteed: rootContainer,
		Burstable:  path.Join(rootContainer, string(v1.PodQOSBurstable)),
		BestEffort: path.Join(rootContainer, string(v1.PodQOSBestEffort)),
	}
	m.getNodeAllocatable = getNodeAllocatable
	m.activePods = activePods

	// update qos cgroup tiers on startup and in periodic intervals
	// to ensure desired state is in synch with actual state.
	go wait.Until(func() {
		err := m.UpdateCgroups()
		if err != nil {
			glog.Warningf("[ContainerManager] Failed to reserve QoS requests: %v", err)
		}
	}, periodicQOSCgroupUpdateInterval, wait.NeverStop)

	return nil
}

func (m *qosContainerManagerImpl) setCPUCgroupConfig(configs map[v1.PodQOSClass]*CgroupConfig) error {
	pods := m.activePods()
	burstablePodCPURequest := int64(0)
	for i := range pods {
		pod := pods[i]
		qosClass := qos.GetPodQOS(pod)
		if qosClass != v1.PodQOSBurstable {
			// we only care about the burstable qos tier
			continue
		}
		req, _, err := v1.PodRequestsAndLimits(pod)
		if err != nil {
			return err
		}
		if request, found := req[v1.ResourceCPU]; found {
			burstablePodCPURequest += request.MilliValue()
		}
	}

	// make sure best effort is always 2 shares
	bestEffortCPUShares := int64(MinShares)
	configs[v1.PodQOSBestEffort].ResourceParameters.CpuShares = &bestEffortCPUShares

	// set burstable shares based on current observe state
	burstableCPUShares := MilliCPUToShares(burstablePodCPURequest)
	if burstableCPUShares < int64(MinShares) {
		burstableCPUShares = int64(MinShares)
	}
	configs[v1.PodQOSBurstable].ResourceParameters.CpuShares = &burstableCPUShares
	return nil
}

// setMemoryReserve sums the memory limits of all pods in a QOS class,
// calculates QOS class memory limits, and set those limits in the
// CgroupConfig for each QOS class.
func (m *qosContainerManagerImpl) setMemoryReserve(configs map[v1.PodQOSClass]*CgroupConfig, percentReserve int64) {
	qosMemoryRequests := map[v1.PodQOSClass]int64{
		v1.PodQOSGuaranteed: 0,
		v1.PodQOSBurstable:  0,
	}

	// Sum the pod limits for pods in each QOS class
	pods := m.activePods()
	for _, pod := range pods {
		podMemoryRequest := int64(0)
		qosClass := qos.GetPodQOS(pod)
		if qosClass == v1.PodQOSBestEffort {
			// limits are not set for Best Effort pods
			continue
		}
		req, _, err := v1.PodRequestsAndLimits(pod)
		if err != nil {
			glog.V(2).Infof("[Container Manager] Pod resource requests/limits could not be determined.  Not setting QOS memory limts.")
			return
		}
		if request, found := req[v1.ResourceMemory]; found {
			podMemoryRequest += request.Value()
		}
		qosMemoryRequests[qosClass] += podMemoryRequest
	}

	resources := m.getNodeAllocatable()
	allocatableResource, ok := resources[v1.ResourceMemory]
	if !ok {
		glog.V(2).Infof("[Container Manager] Allocatable memory value could not be determined.  Not setting QOS memory limts.")
		return
	}
	allocatable := allocatableResource.Value()
	if allocatable == 0 {
		glog.V(2).Infof("[Container Manager] Memory allocatable reported as 0, might be in standalone mode.  Not setting QOS memory limts.")
		return
	}

	for qos, limits := range qosMemoryRequests {
		glog.V(2).Infof("[Container Manager] %s pod requests total %d bytes (reserve %d%%)", qos, limits, percentReserve)
	}

	// Calculate QOS memory limits
	burstableLimit := allocatable - (qosMemoryRequests[v1.PodQOSGuaranteed] * percentReserve / 100)
	bestEffortLimit := burstableLimit - (qosMemoryRequests[v1.PodQOSBurstable] * percentReserve / 100)
	configs[v1.PodQOSBurstable].ResourceParameters.Memory = &burstableLimit
	configs[v1.PodQOSBestEffort].ResourceParameters.Memory = &bestEffortLimit
}

// retrySetMemoryReserve checks for any QoS cgroups over the limit
// that was attempted to be set in the first Update() and adjusts
// their memory limit to the usage to prevent further growth.
func (m *qosContainerManagerImpl) retrySetMemoryReserve(configs map[v1.PodQOSClass]*CgroupConfig, percentReserve int64) {
	// Unreclaimable memory usage may already exceeded the desired limit
	// Attempt to set the limit near the current usage to put pressure
	// on the cgroup and prevent further growth.
	for qos, config := range configs {
		stats, err := m.cgroupManager.GetResourceStats(config.Name)
		if err != nil {
			glog.V(2).Infof("[Container Manager] %v", err)
			return
		}
		usage := stats.MemoryStats.Usage

		// Because there is no good way to determine of the original Update()
		// on the memory resource was successful, we determine failure of the
		// first attempt by checking if the usage is above the limit we attempt
		// to set.  If it is, we assume the first attempt to set the limit failed
		// and try again setting the limit to the usage.  Otherwise we leave
		// the CgroupConfig as is.
		if configs[qos].ResourceParameters.Memory != nil && usage > *configs[qos].ResourceParameters.Memory {
			configs[qos].ResourceParameters.Memory = &usage
		}
	}
}

func (m *qosContainerManagerImpl) UpdateCgroups() error {
	m.Lock()
	defer m.Unlock()

	qosConfigs := map[v1.PodQOSClass]*CgroupConfig{
		v1.PodQOSBurstable: {
			Name:               CgroupName(m.qosContainersInfo.Burstable),
			ResourceParameters: &ResourceConfig{},
		},
		v1.PodQOSBestEffort: {
			Name:               CgroupName(m.qosContainersInfo.BestEffort),
			ResourceParameters: &ResourceConfig{},
		},
	}

	// update the qos level cgroup settings for cpu shares
	if err := m.setCPUCgroupConfig(qosConfigs); err != nil {
		return err
	}

	for resource, percentReserve := range m.qosReserved {
		switch resource {
		case v1.ResourceMemory:
			m.setMemoryReserve(qosConfigs, percentReserve)
		}
	}
	updateSuccess := true
	for _, config := range qosConfigs {
		err := m.cgroupManager.Update(config)
		if err != nil {
			updateSuccess = false
		}
	}
	if updateSuccess {
		glog.V(2).Infof("[ContainerManager]: Updated QoS cgroup configuration")
		return nil
	}

	// If the resource can adjust the ResourceConfig to increase likelihood of
	// success, call the adjustment function here.  Otherwise, the Update() will
	// be called again with the same values.
	for resource, percentReserve := range m.qosReserved {
		switch resource {
		case v1.ResourceMemory:
			m.retrySetMemoryReserve(qosConfigs, percentReserve)
		}
	}

	for _, config := range qosConfigs {
		err := m.cgroupManager.Update(config)
		if err != nil {
			glog.V(2).Infof("[ContainerManager]: Failed to update QoS cgroup configuration")
			return err
		}
	}

	glog.V(2).Infof("[ContainerManager]: Updated QoS cgroup configuration on retry")
	return nil
}

type qosContainerManagerNoop struct {
	cgroupRoot CgroupName
}

var _ QOSContainerManager = &qosContainerManagerNoop{}

func (m *qosContainerManagerNoop) GetQOSContainersInfo() QOSContainersInfo {
	return QOSContainersInfo{}
}

func (m *qosContainerManagerNoop) Start(_ func() v1.ResourceList, _ ActivePodsFunc) error {
	return nil
}

func (m *qosContainerManagerNoop) UpdateCgroups() error {
	return nil
}
