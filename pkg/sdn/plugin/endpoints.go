package plugin

import (
	"fmt"
	"net"
	"os/exec"
	"strings"

	log "github.com/golang/glog"

	kapi "k8s.io/kubernetes/pkg/api"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
	//	"k8s.io/kubernetes/pkg/util/sets"
	utilwait "k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/watch"
)

const (
	monitorCmd         = "monitor"
	unmonitorCmd       = "unmonitor"
	clearMonitoringCmd = "clearmonitoring"
)

type endpointInfo struct {
	ip       string
	port     int32
	protocol kapi.Protocol
}

func (oc *OsdnNode) EndpointStartNode() error {
	go utilwait.Forever(oc.watchEndpoints, 0)
	return nil
}

// Only run on the nodes
func (oc *OsdnNode) watchEndpoints() {
	// This is a map of endpoint UUIDs of sets of endpoint structures (ip addresses, ports, and protocols)
	endpoints := make(map[string]map[endpointInfo]struct{})

	// We also need to track the usage by proto, ip, and port so that we add only one rule
	// even if multiple services point at the same pod
	endpointRefCounts := make(map[endpointInfo]int)

	// Wipe out all existing rules so that the old numbers don't persist across a restart
	oc.clearAllServiceMonitoring()

	// Get the SDN subnet allocated to the node so we can screen endpoint changes
	ourSubnet, err := oc.registry.GetSubnet(oc.hostName)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to determine the subnet for the local host '%s' %v: %v", oc.hostName, ourSubnet, err))
		return
	}
	subnetStr := ourSubnet.Subnet
	_, ourCIDR, err := net.ParseCIDR(subnetStr)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("Unable to convert the subnet for the local host %q, subnet %q: %v", oc.hostName, subnetStr, err))
		return
	}

	// Process the endpoint events and add / remove the monitoring appropriately
	eventQueue := oc.registry.RunEventQueue(Endpoints)

	for {
		eventType, obj, err := eventQueue.Pop()
		if err != nil {
			utilruntime.HandleError(fmt.Errorf("EventQueue failed for endpoints: %v", err))
			return
		}
		ep := obj.(*kapi.Endpoints)
		log.V(5).Infof("Watch %s event for Endpoint %q", strings.Title(string(eventType)), ep.ObjectMeta.Name)

		// Extract the set of endpoints in the record we read
		// For the deleted case, they are the old endpoints and we don't need to process them
		var ourEndpoints = make(map[endpointInfo]struct{})
		if eventType == watch.Added || eventType == watch.Modified {
			for _, ss := range ep.Subsets {
				for _, addr := range ss.Addresses {
					if ourCIDR.Contains(net.ParseIP(addr.IP)) {
						for _, port := range ss.Ports {
							epi := endpointInfo{
								ip:       addr.IP,
								port:     port.Port,
								protocol: port.Protocol,
							}
							ourEndpoints[epi] = struct{}{}
						}
					}
				}
			}
		}

		switch eventType {
		case watch.Added, watch.Modified, watch.Deleted:
			// Pull the old endpoints and set the new ones
			oldEndpoints, exists := endpoints[string(ep.UID)]
			if !exists {
				// Make an empty set so that the code below is the same
				oldEndpoints = make(map[endpointInfo]struct{})
			}

			// Update the endpoint record (or remove it on deletions)
			if eventType == watch.Deleted {
				delete(endpoints, string(ep.UID))
			} else {
				endpoints[string(ep.UID)] = ourEndpoints
			}

			// Delete the endpoint rules when the old endpoints had a rule, but the new doesn't
			for epi := range oldEndpoints {
				_, exists := ourEndpoints[epi]
				if !exists {
					endpointRefCounts[epi]--
					if endpointRefCounts[epi] == 0 {
						err := oc.deleteServiceMonitoring(epi)
						if err != nil {
							utilruntime.HandleError(fmt.Errorf("deleteServiceMonitoring failed for endpoints %v: %v", epi, err))
						}
					} else if endpointRefCounts[epi] < 0 {
						// The ref counts should never be negative!  Handle it by setting it to 0 and ignoring
						utilruntime.HandleError(fmt.Errorf("Got an unexpected negative refcount for %v", epi))
						endpointRefCounts[epi] = 0
					}
				}
			}

			// Add endpoint rules when the new endpoints have a rule, but the old doesn't
			for epi := range ourEndpoints {
				_, exists := oldEndpoints[epi]
				if !exists {
					endpointRefCounts[epi]++
					if endpointRefCounts[epi] == 1 {
						err := oc.addServiceMonitoring(epi)
						if err != nil {
							utilruntime.HandleError(fmt.Errorf("addServiceMonitoring failed for endpoints %v: %v", epi, err))
						}
					}
				}
			}

		}
	}
}

// clearAllServiceMonitoring erases all service monitoring rules from the system.
// Specifically, it finds all OVS rules that were created for service monitoring
// by OpenShift and removes them.
func (oc *OsdnNode) clearAllServiceMonitoring() error {
	// The script's teardown functionality doesn't need a container ID or VNID
	out, err := exec.Command(oc.getExecutable(), clearMonitoringCmd).CombinedOutput()
	log.V(5).Infof("ClearAllServiceMonitoring network plugin output: %s, %v", string(out), err)

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
func (oc *OsdnNode) addServiceMonitoring(epi endpointInfo) error {
	// The script's teardown functionality doesn't need a container ID or VNID

	out, err := exec.Command(oc.getExecutable(), monitorCmd, "-1", "-1", "-1", "-1", "-1", epi.ip, strings.ToLower(string(epi.protocol)), fmt.Sprintf("%d", epi.port)).CombinedOutput()
	log.V(5).Infof("addServiceMonitoring network plugin output: %s, %v", string(out), err)

	if isScriptError(err) {
		return fmt.Errorf("Error running add network port monitoring script: %s", getScriptError(out))
	} else {
		return err
	}
}

// deleteServiceMonitoring removes a IP port monitoring rule for a pod.
// This is implemented by deleting the OVS rule that monitors the port.
func (oc *OsdnNode) deleteServiceMonitoring(epi endpointInfo) error {
	// The script's teardown functionality doesn't need a container ID or VNID

	out, err := exec.Command(oc.getExecutable(), unmonitorCmd, "-1", "-1", "-1", "-1", "-1", epi.ip, strings.ToLower(string(epi.protocol)), fmt.Sprintf("%d", epi.port)).CombinedOutput()
	log.V(5).Infof("deleteServiceMonitoring network plugin output: %s, %v", string(out), err)

	if isScriptError(err) {
		return fmt.Errorf("Error running network port unmonitoring script: %s", getScriptError(out))
	} else {
		return err
	}
}
