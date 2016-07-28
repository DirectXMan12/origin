package statscollector

import (
	"fmt"
	"strconv"
	"strings"

	sdnplugin "github.com/openshift/origin/pkg/sdn/plugin"
	"github.com/openshift/origin/pkg/util/ovs"
	"k8s.io/kubernetes/pkg/util/exec"
)

type ServicePortStats struct {
	Packets uint64
	Bytes   uint64
}

type IPPort struct {
	IP   string
	Port int
}

type IPPortStats map[IPPort]ServicePortStats

type PortStatsCollector interface {
	// FetchStats collects packet and byte counts for the all of the
	// Pod IPs on each service port on this node.
	CollectStats() (IPPortStats, error)
}

func NewOVSStatsCollector(execer exec.Interface) PortStatsCollector {
	return &ovsPortStatsCollector{
		execer: execer,
	}
}

type ovsPortStatsCollector struct {
	execer exec.Interface
}

func (c *ovsPortStatsCollector) CollectStats() (IPPortStats, error) {
	tx := ovs.NewTransaction(c.execer, sdnplugin.BR)
	flows, err := tx.DumpFlowsFilter("table=8")
	if err != nil {
		return nil, fmt.Errorf("unable to fetch OVS flows to collect stats: %v", err)
	}

	var stats IPPortStats
	stats, err = parseOVSRules(flows)

	if err != nil {
		return nil, fmt.Errorf("unable to extract stats from OVS flows: %v", err)
	}

	return stats, nil
}

func parseOVSRules(lines []string) (IPPortStats, error) {
	ipPortData := make(IPPortStats)

	// Ignore the header line
	for _, line := range lines {
		// Get rid of actions= ... at the end
		junk := strings.Split(line, " actions=")
		line = junk[0]

		// See if the line is blank after our cleaning
		if line == "" {
			continue
		}

		// Lose the leading spaces
		line := strings.Trim(line, " ")

		data := make(map[string]string)

		pairs := strings.Split(line, ",")
		for _, pair := range pairs {
			pair = strings.Trim(pair, " ")
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) == 2 {
				data[kv[0]] = kv[1]
			} else {
				data[kv[0]] = ""
			}
		}

		ip, found := data["nw_dst"]
		if !found {
			// If there's no IP, ignore it and move on to the next line
			continue
		}

		portStr, found := data["tp_dst"]
		if !found {
			portStr = "0"
		}

		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("Bad port %q in line %q: %v", port, line, err)
		}
		packets, err := strconv.ParseUint(data["n_packets"], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("Bad packet count %q in line %q: %v", data["n_packets"], line, err)
		}
		bytes, err := strconv.ParseUint(data["n_bytes"], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("Bad byte count %q in line %q: %v", data["n_bytes"], line, err)
		}

		ipPortData[IPPort{ip, port}] = ServicePortStats{
			Packets: packets,
			Bytes:   bytes,
		}
	}

	return ipPortData, nil
}
