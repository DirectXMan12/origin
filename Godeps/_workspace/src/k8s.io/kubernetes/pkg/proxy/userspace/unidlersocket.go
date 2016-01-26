/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package userspace

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/proxy"
	"k8s.io/kubernetes/pkg/util/unidling"
)

type connectionList struct {
	conns   []heldConn
	maxSize int

	tickSize       time.Duration
	timeSinceStart time.Duration
	timeout        time.Duration

	svcName string
}

type heldConn struct {
	net.Conn
	connectedAt time.Duration
}

func newConnectionList(maxSize int, tickSize time.Duration, timeout time.Duration, svcName string) *connectionList {
	return &connectionList{
		conns:          []heldConn{},
		maxSize:        maxSize,
		tickSize:       tickSize,
		timeSinceStart: 0,
		timeout:        timeout,
		svcName:        svcName,
	}
}

func (l *connectionList) Add(conn net.Conn) {
	if len(l.conns) >= l.maxSize {
		// TODO: look for closed connections
		glog.Errorf("max connections exceeded while waiting for idled service %s to awaken, dropping oldest", l.svcName)
		var oldConn net.Conn
		oldConn, l.conns = l.conns[0], l.conns[1:]
		oldConn.Close()
	}

	l.conns = append(l.conns, heldConn{conn, l.timeSinceStart})
}

func (l *connectionList) Tick() {
	l.timeSinceStart += l.tickSize
	l.cleanOldConnections()
}

func (l *connectionList) cleanOldConnections() {
	cleanInd := -1
	for i, conn := range l.conns {
		if l.timeSinceStart-conn.connectedAt < l.timeout {
			cleanInd = i
			break
		}
	}

	if cleanInd > 0 {
		oldConns := l.conns[:cleanInd]
		l.conns = l.conns[cleanInd:]
		glog.Errorf("timed out %v connections while waiting for idled service %s to awaken.", len(oldConns), l.svcName)

		for _, conn := range oldConns {
			conn.Close()
		}
	}
}

func (l *connectionList) GetConns() []net.Conn {
	conns := make([]net.Conn, len(l.conns))
	for i, conn := range l.conns {
		conns[i] = conn.Conn
	}
	return conns
}

func (l *connectionList) Len() int {
	return len(l.conns)
}

func (l *connectionList) Clear() {
	for _, conn := range l.conns {
		conn.Close()
	}

	l.conns = []heldConn{}
}

var (
	needPodsWaitTimeout = 30 * time.Second
	needPodsTickLen     = 5 * time.Second
	maxHeldConnections  = 16
)

func newUnidlerSocket(protocol api.Protocol, ip net.IP, port int, eventRecorder record.EventRecorder) (proxySocket, error) {
	host := ""
	if ip != nil {
		host = ip.String()
	}

	switch strings.ToUpper(string(protocol)) {
	case "TCP":
		listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		proxySocket := tcpProxySocket{Listener: listener, port: port}
		return &tcpUnidlerSocket{tcpProxySocket: proxySocket, eventRecorder: eventRecorder}, nil
	case "UDP":
		addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			return nil, err
		}
		proxySocket := udpProxySocket{UDPConn: conn, port: port}
		return &udpUnidlerSocket{udpProxySocket: proxySocket, eventRecorder: eventRecorder}, nil
	}
	return nil, fmt.Errorf("unknown protocol %q", protocol)
}

// tcpUnidlerSocket implements proxySocket.  Close() is implemented by net.Listener.  When Close() is called,
// no new connections are allowed but existing connections are left untouched.
type tcpUnidlerSocket struct {
	tcpProxySocket
	eventRecorder record.EventRecorder
}

func (tcp *tcpUnidlerSocket) waitForEndpoints(ch chan<- interface{}, service proxy.ServicePortName, proxier *Proxier) {
	defer close(ch)
	for {
		if proxier.loadBalancer.ServiceHasEndpoints(service) {
			// we have endpoints now, so we're finished
			return
		}

		// otherwise, wait a bit before checking for endpoints again
		time.Sleep(endpointDialTimeout[0])
	}
}

func (tcp *tcpUnidlerSocket) acceptConns(ch chan<- net.Conn, myInfo *serviceInfo) {
	defer close(ch)

	// Block until a connection is made.
	for {
		inConn, err := tcp.Accept()
		if err != nil {
			if isTooManyFDsError(err) {
				panic("Accept failed: " + err.Error())
			}

			// TODO: indicate errors here?
			if isClosedError(err) {
				return
			}
			if !myInfo.isAlive() {
				// Then the service port was just closed so the accept failure is to be expected.
				return
			}
			glog.Errorf("Accept failed: %v", err)
			continue
		}

		ch <- inConn
	}
}

func (tcp *tcpUnidlerSocket) ProxyLoop(service proxy.ServicePortName, myInfo *serviceInfo, proxier *Proxier) {
	if !myInfo.isAlive() {
		// The service port was closed or replaced.
		return
	}

	// accept connections asynchronously
	inConns := make(chan net.Conn)
	go tcp.acceptConns(inConns, myInfo)

	endpointsAvail := make(chan interface{})
	var allConns *connectionList

	svcName := fmt.Sprintf("%s/%s:%s", service.Namespace, service.Name, service.Port)

TOP_LOOP:
	for {
		glog.V(4).Infof("unidling TCP proxy start/reset for service %s/%s:%s", service.Namespace, service.Name, service.Port)
		// collect connections and wait for endpoints to be available
		sent_need_pods := false
		timeout_started := false
		ticker := time.NewTicker(needPodsTickLen)
		allConns = newConnectionList(maxHeldConnections, needPodsTickLen, needPodsWaitTimeout, svcName)

	WAIT_LOOP:
		for {
			select {
			case inConn, ok := <-inConns:
				if !ok {
					// the listen socket has been closed, so we're finished accepting connections
					break TOP_LOOP
				}

				if !sent_need_pods && !proxier.loadBalancer.ServiceHasEndpoints(service) {
					// TODO: There's a constant in origin for this NeedPods
					// HACK: make the message different to prevent event aggregation
					glog.V(4).Infof("unidling TCP proxy sent unidle event to wake up service %s/%s:%s", service.Namespace, service.Name, service.Port)
					tcp.eventRecorder.Eventf(myInfo.ref, api.EventTypeNormal, unidling.NeedPodsReason, "The service-port %s/%s:%s needs pods (at %v)!", service.Namespace, service.Name, service.Port, time.Now())

					// only send NeedPods once
					sent_need_pods = true
					timeout_started = true
				}

				if allConns.Len() == 0 {
					if !proxier.loadBalancer.ServiceHasEndpoints(service) {
						// notify us when endpoints are available
						go tcp.waitForEndpoints(endpointsAvail, service, proxier)
					}
				}

				allConns.Add(inConn)
				glog.V(4).Infof("unidling TCP proxy has accumulated %v connections while waiting for service %s/%s:%s to unidle", allConns.Len(), service.Namespace, service.Name, service.Port)
			case <-ticker.C:
				if !timeout_started {
					continue WAIT_LOOP
				}
				// TODO: timeout each connection (or group of connections) separately
				// timed out, close all waiting connections and reset the state
				allConns.Tick()
			}
		}

		ticker.Stop()
	}

	glog.V(4).Infof("unidling TCP proxy waiting for endpoints for service %s/%s:%s to become available with %v accumulated connections", service.Namespace, service.Name, service.Port, allConns.Len())
	// block until we have endpoints available
	select {
	case _, ok := <-endpointsAvail:
		if ok {
			close(endpointsAvail)
			// this shouldn't happen (ok should always be false)
		}
	case <-time.NewTimer(needPodsWaitTimeout).C:
		if allConns.Len() > 0 {
			glog.Errorf("timed out %v TCP connections while waiting for idled service %s/%s:%s to awaken.", allConns.Len(), service.Namespace, service.Name, service.Port)
			allConns.Clear()
		}
		return
	}
	glog.V(4).Infof("unidling TCP proxy got endpoints for service %s/%s:%s, connecting %v accumulated connections", service.Namespace, service.Name, service.Port, allConns.Len())

	for _, inConn := range allConns.GetConns() {
		glog.V(3).Infof("Accepted TCP connection from %v to %v", inConn.RemoteAddr(), inConn.LocalAddr())
		outConn, err := tryConnect(service, inConn.(*net.TCPConn).RemoteAddr(), "tcp", proxier)
		if err != nil {
			glog.Errorf("Failed to connect to balancer: %v", err)
			inConn.Close()
			continue
		}
		// Spin up an async copy loop.
		go proxyTCP(inConn.(*net.TCPConn), outConn.(*net.TCPConn))
	}
}

// udpUnidlerSocket implements proxySocket.  Close() is implemented by net.UDPConn.  When Close() is called,
// no new connections are allowed and existing connections are broken.
// TODO: We could lame-duck this ourselves, if it becomes important.
type udpUnidlerSocket struct {
	udpProxySocket
	eventRecorder record.EventRecorder
}

func (udp *udpUnidlerSocket) readFromSock(buffer []byte, myInfo *serviceInfo) bool {
	if !myInfo.isAlive() {
		// The service port was closed or replaced.
		return false
	}

	// Block until data arrives.
	// TODO: Accumulate a histogram of n or something, to fine tune the buffer size.
	_, _, err := udp.ReadFrom(buffer)
	if err != nil {
		if e, ok := err.(net.Error); ok {
			if e.Temporary() {
				glog.V(1).Infof("ReadFrom had a temporary failure: %v", err)
				return true
			}
		}
		glog.Errorf("ReadFrom failed, exiting ProxyLoop: %v", err)
		return false
	}

	return true
}

func (udp *udpUnidlerSocket) sendWakeup(service proxy.ServicePortName, myInfo *serviceInfo) (chan<- interface{}, *time.Timer) {
	stopChan := make(chan interface{})
	timeoutTimer := time.NewTimer(needPodsWaitTimeout)

	go func(stop <-chan interface{}, timeout <-chan time.Time) {
		select {
		case <-stop:
			return
		case <-timeout:
			return
		case <-time.NewTicker(needPodsWaitTimeout).C:
			glog.V(4).Infof("unidling proxy sent unidle event to wake up service %s/%s:%s", service.Namespace, service.Name, service.Port)
			udp.eventRecorder.Eventf(myInfo.ref, unidling.NeedPodsReason, "The service-port %s/%s:%s needs pods (at %v)!", service.Namespace, service.Name, service.Port, time.Now())
		}
	}(stopChan, timeoutTimer.C)

	return stopChan, timeoutTimer
}

func (udp *udpUnidlerSocket) ProxyLoop(service proxy.ServicePortName, myInfo *serviceInfo, proxier *Proxier) {
	// just drop the packets on the floor until we have endpoints
	var buffer [4096]byte // 4KiB should be enough for most whole-packets

	glog.V(4).Infof("unidling proxy UDP proxy waiting for data")
	if !udp.readFromSock(buffer[0:], myInfo) {
		return
	}

	stopChan, wakeupTimeoutTimer := udp.sendWakeup(service, myInfo)
	defer close(stopChan)

	for {
		if !udp.readFromSock(buffer[0:], myInfo) {
			break
		}
		if active := wakeupTimeoutTimer.Reset(needPodsWaitTimeout); !active {
			stopChan, wakeupTimeoutTimer = udp.sendWakeup(service, myInfo)
		}
	}
}
