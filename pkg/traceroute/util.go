// Copyright 2024 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package traceroute

import (
	"bytes"
	"fmt"
	"net"
)

type coms struct {
	sendChan chan *Probe
	recvChan chan *Probe
	exitChan chan bool
}

// Given a host name convert it to a 4 byte IP address.
func destAddr(dest string) (destAddr [4]byte, err error) {
	addrs, err := net.LookupHost(dest)
	if err != nil {
		return
	}
	addr := addrs[0]

	ipAddr, err := net.ResolveIPAddr("ip", addr)
	if err != nil {
		return
	}
	copy(destAddr[:], ipAddr.IP.To4())
	return
}

func findDestinationTTL(printMap map[int]*Probe, dest [4]byte) int {
	for _, pb := range printMap {
		if bytes.Equal([]byte(pb.saddr), dest[:]) {
			fmt.Println(pb)
			return pb.ttl
		}
	}
	return DEFNUMHOPS
}

func getProbesByTLL(printMap map[int]*Probe, ttl int) []*Probe {
	pbs := make([]*Probe, 0)
	for _, pb := range printMap {
		if pb.ttl == ttl {
			pbs = append(pbs, pb)
		}
	}
	return pbs
}