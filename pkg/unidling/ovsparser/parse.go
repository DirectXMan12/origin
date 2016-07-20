package ovsparser

import (
	"log"
	"strconv"
	"strings"
)

type portStats struct {
	packets uint64
	bytes   uint64
}

type IPPortStats map[string]map[int]portStats

func Parse(raw []byte) (IPPortStats, error) {

	ipPortData := make(IPPortStats)

	// Ignore the header line
	lines := strings.Split(string(raw), "\n")[1:]

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
			log.Printf("Bad port %s\n\n%#v", line, data)
			return nil, err
		}
		packets, err := strconv.ParseUint(data["n_packets"], 10, 64)
		if err != nil {
			log.Printf("Bad packets: %s\n\n%#v", line, data)
			return nil, err
		}
		bytes, err := strconv.ParseUint(data["n_bytes"], 10, 64)
		if err != nil {
			log.Printf("Bad bytes: %s\n\n%#v", line, data)
			return nil, err
		}

		_, found = ipPortData[ip]
		if !found {
			ipPortData[ip] = make(map[int]portStats)
		}

		ipPortData[ip][port] = portStats{
			packets: packets,
			bytes:   bytes,
		}
	}

	return ipPortData, nil
}
