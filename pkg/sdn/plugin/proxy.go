package plugin

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"

	"github.com/golang/glog"

	osclient "github.com/openshift/origin/pkg/client"
	"github.com/openshift/origin/pkg/sdn/plugin/api"

	kapi "k8s.io/kubernetes/pkg/api"
	kclient "k8s.io/kubernetes/pkg/client/unversioned"
	pconfig "k8s.io/kubernetes/pkg/proxy/config"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	utilwait "k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/watch"
)

type endpointInfo struct {
	ip       string
	port     int32
	protocol kapi.Protocol
}

type ovsProxyPlugin struct {
	registry       *Registry
	podsByIP       map[string]*kapi.Pod
	podsMutex      sync.Mutex
	endpoints      map[endpointInfo]struct{}
	isMultitenant  bool
	disableMetrics bool
	hostName       string

	baseEndpointsHandler pconfig.EndpointsConfigHandler
}

const (
	clearMonitoringCmd = "clearmonitoring"
	monitorCmd         = "monitor"
	unmonitorCmd       = "unmonitor"
)

//BBXXX TODO:
// - Want to instrument egress too

// Called by higher layers to create the proxy plugin instance; only used by nodes
func NewProxyPlugin(pluginName string, osClient *osclient.Client, kClient *kclient.Client, hostname string, disableMetrics bool) (api.FilteringEndpointsConfigHandler, error) {
	if !IsOpenShiftNetworkPlugin(pluginName) {
		return nil, nil
	}

	isMultitenant := IsOpenShiftMultitenantNetworkPlugin(pluginName)

	if disableMetrics {
		glog.Infof("OpenShift plugin %s configured without port metrics enabled", pluginName)

		// If they aren't multitenant and they don't want metrics... then there's nothing to do
		if !isMultitenant {
			return nil, nil
		}
	} else {
		glog.Infof("OpenShift plugin %s configured with port metrics enabled", pluginName)
	}

	proxy := &ovsProxyPlugin{
		registry:       newRegistry(osClient, kClient),
		podsByIP:       make(map[string]*kapi.Pod),
		endpoints:      make(map[endpointInfo]struct{}),
		isMultitenant:  isMultitenant,
		disableMetrics: disableMetrics,
		hostName:       hostname,
	}

	// Wipe out all existing rules so that the old stats don't persist across a restart
	if !disableMetrics {
		proxy.clearAllServiceMonitoring()
	}

	return proxy, nil
}

func (proxy *ovsProxyPlugin) Start(baseHandler pconfig.EndpointsConfigHandler) error {
	if !proxy.isMultitenant {
		// We don't start any watchers when we are not multitenant
		return nil
	}

	glog.Infof("Starting multitenant SDN proxy endpoint filter")

	proxy.baseEndpointsHandler = baseHandler

	// Populate pod info map synchronously so that kube proxy can filter endpoints to support isolation
	pods, err := proxy.registry.GetAllPods()
	if err != nil {
		return err
	}

	for _, pod := range pods {
		proxy.trackPod(&pod)
	}

	go utilwait.Forever(proxy.watchPods, 0)

	return nil
}

func (proxy *ovsProxyPlugin) watchPods() {
	eventQueue := proxy.registry.RunEventQueue(Pods)

	for {
		eventType, obj, err := eventQueue.Pop()
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("EventQueue failed for pods: %v", err))
			return
		}
		pod := obj.(*kapi.Pod)

		glog.V(5).Infof("Watch %s event for Pod %q", strings.Title(string(eventType)), pod.ObjectMeta.Name)
		switch eventType {
		case watch.Added, watch.Modified:
			proxy.trackPod(pod)
		case watch.Deleted:
			proxy.unTrackPod(pod)
		}
	}
}

func (proxy *ovsProxyPlugin) getTrackedPod(ip string) (*kapi.Pod, bool) {
	proxy.podsMutex.Lock()
	defer proxy.podsMutex.Unlock()

	pod, ok := proxy.podsByIP[ip]
	return pod, ok
}

func (proxy *ovsProxyPlugin) trackPod(pod *kapi.Pod) {
	if pod.Status.PodIP == "" {
		return
	}

	proxy.podsMutex.Lock()
	defer proxy.podsMutex.Unlock()
	podInfo, ok := proxy.podsByIP[pod.Status.PodIP]

	if pod.Status.Phase == kapi.PodPending || pod.Status.Phase == kapi.PodRunning {
		// When a pod hits one of the states where the IP is in use then
		// we need to add it to our IP -> namespace tracker.  There _should_ be no
		// other entries for the IP if we caught all of the right messages, so warn
		// if we see one, but clobber it anyway since the IPAM
		// should ensure that each IP is uniquely assigned to a pod (when running)
		if ok && podInfo.UID != pod.UID {
			glog.Warningf("IP '%s' was marked as used by namespace '%s' (pod '%s')... updating to namespace '%s' (pod '%s')",
				pod.Status.PodIP, podInfo.Namespace, podInfo.UID, pod.ObjectMeta.Namespace, pod.UID)
		}

		proxy.podsByIP[pod.Status.PodIP] = pod
	} else if ok && podInfo.UID == pod.UID {
		// If the UIDs match, then this pod is moving to a state that indicates it is not running
		// so we need to remove it from the cache
		delete(proxy.podsByIP, pod.Status.PodIP)
	}
}

func (proxy *ovsProxyPlugin) unTrackPod(pod *kapi.Pod) {
	proxy.podsMutex.Lock()
	defer proxy.podsMutex.Unlock()

	// Only delete if the pod ID is the one we are tracking (in case there is a failed or complete
	// pod lying around that gets deleted while there is a running pod with the same IP)
	if podInfo, ok := proxy.podsByIP[pod.Status.PodIP]; ok && podInfo.UID == pod.UID {
		delete(proxy.podsByIP, pod.Status.PodIP)
	}
}

func (proxy *ovsProxyPlugin) OnEndpointsUpdate(allEndpoints []kapi.Endpoints) {
	ni, err := proxy.registry.GetNetworkInfo()
	if err != nil {
		glog.Warningf("Error fetching network information: %v", err)
		return
	}

	// Get the SDN subnet allocated to the node so we can screen endpoint changes
	ourSubnet, err := proxy.registry.GetSubnet(proxy.hostName)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to determine the subnet for the local host '%s': %v", proxy.hostName))
		return
	}
	subnetStr := ourSubnet.Subnet
	_, ourCIDR, err := net.ParseCIDR(subnetStr)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to convert the subnet for the local subnet %q: %v", subnetStr, err))
		return
	}

	filteredEndpoints := make([]kapi.Endpoints, 0, len(allEndpoints))
	newEndpoints := make(map[endpointInfo]struct{})

EndpointLoop:
	for _, ep := range allEndpoints {
		ns := ep.ObjectMeta.Namespace
		for _, ss := range ep.Subsets {
			for _, addr := range ss.Addresses {
				IP := net.ParseIP(addr.IP)
				if ni.ServiceNetwork.Contains(IP) {
					glog.Warningf("Service '%s' in namespace '%s' has an Endpoint inside the service network (%s)", ep.ObjectMeta.Name, ns, addr.IP)
					continue EndpointLoop
				}
				if ni.ClusterNetwork.Contains(IP) {
					// We only need to check the namespace for the multitenant plugin
					if proxy.isMultitenant {
						podInfo, ok := proxy.getTrackedPod(addr.IP)
						if !ok {
							glog.Warningf("Service '%s' in namespace '%s' has an Endpoint pointing to non-existent pod (%s)", ep.ObjectMeta.Name, ns, addr.IP)
							continue EndpointLoop
						}
						if podInfo.ObjectMeta.Namespace != ns {
							glog.Warningf("Service '%s' in namespace '%s' has an Endpoint pointing to pod %s in namespace '%s'", ep.ObjectMeta.Name, ns, addr.IP, podInfo.ObjectMeta.Namespace)
							continue EndpointLoop
						}
					}
					// See if it is on our node, and if so, track the endpoints so we can monitor them
					if !proxy.disableMetrics && ourCIDR.Contains(net.ParseIP(addr.IP)) {
						for _, port := range ss.Ports {
							epi := endpointInfo{
								ip:       addr.IP,
								port:     port.Port,
								protocol: port.Protocol,
							}
							newEndpoints[epi] = struct{}{}
						}
					}

				}
			}
		}
		filteredEndpoints = append(filteredEndpoints, ep)
	}

	if !proxy.disableMetrics {
		proxy.applyEndpointChanges(newEndpoints)
	}

	proxy.baseEndpointsHandler.OnEndpointsUpdate(filteredEndpoints)
}

func (proxy *ovsProxyPlugin) applyEndpointChanges(newEndpoints map[endpointInfo]struct{}) {
	// Pull the old endpoints and set the new ones
	oldEndpoints := proxy.endpoints
	proxy.endpoints = newEndpoints

	// Delete the endpoint rules when the old endpoints had a rule, but the new doesn't
	for epi, _ := range oldEndpoints {
		_, exists := newEndpoints[epi]
		if !exists {
			err := proxy.deleteServiceMonitoring(epi)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("DeleteServiceMonitoring failed for endpoints %v: %v", epi, err))
			}
		}
	}

	// Add endpoint rules when the new endpoints have a rule, but the old doesn't
	for epi, _ := range newEndpoints {
		_, exists := oldEndpoints[epi]
		if !exists {
			err := proxy.addServiceMonitoring(epi)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("AddServiceMonitoring failed for endpoints %v: %v", epi, err))
			}
		}
	}

}

// clearAllServiceMonitoring erases all service monitoring rules from the system.
// Specifically, it finds all OVS rules that were created for service monitoring
// by OpenShift and removes them.
func (proxy *ovsProxyPlugin) clearAllServiceMonitoring() error {
	// The script's teardown functionality doesn't need a container ID or VNID
	out, err := exec.Command(PluginExecutable, clearMonitoringCmd).CombinedOutput()
	glog.V(5).Infof("ClearAllServiceMonitoring network plugin output: %s, %v", string(out), err)

	if isScriptError(err) {
		return fmt.Errorf("Error running clear network port monitoring script: %s", getScriptError(out))
	} else {
		return err
	}
}

// addServiceMonitoring adds a IP port monitoring rule for a pod.
// This is implemented by adding an OVS rule for the OVS port for the
// given IP port so that the statistics can be gatherered for traffic
// entering the pod on that port.  It is intended to be placed on all
// ports that are used by services.
func (proxy *ovsProxyPlugin) addServiceMonitoring(ep endpointInfo) error {
	// The script's teardown functionality doesn't need a container ID or VNID

	out, err := exec.Command(PluginExecutable, monitorCmd, "-1", "-1", "-1", "-1", "-1", ep.ip, strings.ToLower(string(ep.protocol)), fmt.Sprintf("%d", ep.port)).CombinedOutput()
	glog.V(5).Infof("AddServiceMonitoring network plugin output: %s, %v", string(out), err)

	if isScriptError(err) {
		return fmt.Errorf("Error running add network port monitoring script: %s", getScriptError(out))
	} else {
		return err
	}
}

// DeleteServiceMonitoring removes a IP port monitoring rule for a pod.
// This is implemented by deleting the OVS rule that monitors the port.
func (proxy *ovsProxyPlugin) deleteServiceMonitoring(ep endpointInfo) error {
	// The script's teardown functionality doesn't need a container ID or VNID

	out, err := exec.Command(PluginExecutable, unmonitorCmd, "-1", "-1", "-1", "-1", "-1", ep.ip, strings.ToLower(string(ep.protocol)), fmt.Sprintf("%d", ep.port)).CombinedOutput()
	glog.V(5).Infof("DeleteServiceMonitoring network plugin output: %s, %v", string(out), err)

	if isScriptError(err) {
		return fmt.Errorf("Error running network port unmonitoring script: %s", getScriptError(out))
	} else {
		return err
	}
}
