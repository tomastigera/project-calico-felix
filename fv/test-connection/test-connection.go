// Copyright (c) 2020 Tigera, Inc. All rights reserved.
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

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/docopt/docopt-go"
	"github.com/ishidawataru/sctp"
	reuse "github.com/libp2p/go-reuseport"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/felix/fv/cgroup"
	"github.com/projectcalico/felix/fv/connectivity"
	"github.com/projectcalico/felix/fv/utils"
)

const usage = `test-connection: test connection to some target, for Felix FV testing.

Usage:
  test-connection <namespace-path> <ip-address> <port> [--source-ip=<source_ip>] [--source-port=<source>] [--protocol=<protocol>] [--loop-with-file=<file>] [--sendlen=<bytes>] [--recvlen=<bytes>]

Options:
  --source-ip=<source_ip>  Source IP to use for the connection [default: 0.0.0.0].
  --source-port=<source_port>  Source port to use for the connection [default: 0].
  --protocol=<protocol>   Protocol to test tcp (default), udp (connected) udp-noconn (unconnected).
  --loop-with-file=<file>  Whether to send messages repeatedly, file is used for synchronization
  --sendlen=<bytes> How many additional bytes to send
  --recvlen=<bytes> Tell the other side to send this many additional bytes

If connection is successful, test-connection exits successfully.

If connection is unsuccessful, test-connection panics and so exits with a failure status.`

// Note about the --loop-with-file=<FILE> flag:
//
// This flag takes a path to a file as a value. The file existence is
// used as a means of synchronization.
//
// Before this program is started, the file should exist. When the
// program establishes a long-running connection and sends the first
// message, it will remove the file. That way other process can assume
// that the connection is here when the file disappears and can
// perform some checks.
//
// If the other process creates the file again, it will tell this
// program to close the connection, remove the file and quit.

const defaultIPv4SourceIP = "0.0.0.0"
const defaultIPv6SourceIP = "::"

func main() {
	log.SetLevel(log.DebugLevel)

	// If we've been told to, move into this felix's cgroup.
	cgroup.MaybeMoveToFelixCgroupv2()

	arguments, err := docopt.ParseArgs(usage, nil, "v0.1")
	if err != nil {
		println(usage)
		log.WithError(err).Fatal("Failed to parse usage")
	}
	log.WithField("args", arguments).Info("Parsed arguments")
	namespacePath := arguments["<namespace-path>"].(string)
	ipAddress := arguments["<ip-address>"].(string)
	port := arguments["<port>"].(string)
	sourcePort := arguments["--source-port"].(string)
	sourceIpAddress := arguments["--source-ip"].(string)
	sendLenStr := arguments["--sendlen"].(string)
	recvLenStr := arguments["--recvlen"].(string)

	sendLen := 0
	if sendLenStr != "" {
		sendLen, _ = strconv.Atoi(sendLenStr)
	}
	recvLen := 0
	if recvLenStr != "" {
		recvLen, _ = strconv.Atoi(recvLenStr)
	}

	// Set default for source IP. If we're using IPv6 as indicated by ipAddress
	// and no --source-ip option was provided, set the source IP to the default
	// IPv6 address.
	if strings.Contains(ipAddress, ":") && sourceIpAddress == defaultIPv4SourceIP {
		sourceIpAddress = defaultIPv6SourceIP
	}

	log.Infof("Test connection from namespace %v IP %v port%v to IP %v port %v", namespacePath, sourceIpAddress, sourcePort, ipAddress, port)
	protocol := arguments["--protocol"].(string)
	loopFile := ""
	if arg, ok := arguments["--loop-with-file"]; ok && arg != nil {
		loopFile = arg.(string)
	}

	if loopFile == "" {
		// I found that configuring the timeouts on all the
		// network calls was a bit fiddly.  Since it leaves
		// the process hung if one of them is missed, use a
		// global timeout instead.
		go func() {
			time.Sleep(2 * time.Second)
			log.Fatal("Timed out")
		}()
	}

	try := try{ipAddress, port, sourceIpAddress, sourcePort, protocol, loopFile, sendLen, recvLen}

	if namespacePath == "-" {
		// Add the source IP (if set) to eth0.
		err = maybeAddAddr(sourceIpAddress)
		// Test connection from wherever we are already running.
		if err == nil {
			err = try.Connect()
		}
	} else {
		// Get the specified network namespace (representing a workload).
		var namespace ns.NetNS
		namespace, err = ns.GetNS(namespacePath)
		if err != nil {
			panic(err)
		}
		log.WithField("namespace", namespace).Debug("Got namespace")

		// Now, in that namespace, try connecting to the target.
		err = namespace.Do(func(_ ns.NetNS) error {
			// Add an interface for the source IP if any.
			e := maybeAddAddr(sourceIpAddress)
			if e != nil {
				return e
			}
			return try.Connect()
		})
	}

	if err != nil {
		panic(err)
	}
}

func maybeAddAddr(sourceIP string) error {
	if sourceIP != defaultIPv4SourceIP && sourceIP != defaultIPv6SourceIP {
		if !strings.Contains(sourceIP, ":") {
			sourceIP += "/32"
		} else {
			sourceIP += "/128"
		}

		// Check if the IP is already set on eth0.
		out, err := exec.Command("ip", "a", "show", "dev", "eth0").Output()
		if err != nil {
			return err
		}
		if strings.Contains(string(out), sourceIP) {
			log.Infof("IP addr %s already exists on eth0, skip adding IP", sourceIP)
			return nil
		}
		cmd := exec.Command("ip", "addr", "add", sourceIP, "dev", "eth0")
		return cmd.Run()
	}
	return nil
}

type try struct {
	remoteIpAddr string
	remotePort   string
	sourceIpAddr string
	sourcePort   string
	protocol     string
	loopFile     string
	sendLen      int
	recvLen      int
}

func (t *try) Connect() error {

	err := utils.RunCommand("ip", "r")
	if err != nil {
		return err
	}

	var localAddr string
	var remoteAddr string
	if strings.Contains(t.remoteIpAddr, ":") {
		localAddr = "[" + t.sourceIpAddr + "]:" + t.sourcePort
		remoteAddr = "[" + t.remoteIpAddr + "]:" + t.remotePort
	} else {
		localAddr = t.sourceIpAddr + ":" + t.sourcePort
		remoteAddr = t.remoteIpAddr + ":" + t.remotePort
	}
	ls := newLoopState(t.loopFile)
	log.Infof("Connecting from %v to %v over %s", localAddr, remoteAddr, t.protocol)

	switch t.protocol {
	case "udp":
		t.udp(ls, localAddr, remoteAddr)
	case "udp-recvmsg":
		t.udpConnectedRecvmsg(ls, localAddr, remoteAddr)
	case "udp-noconn":
		t.udpUnConnected(ls, localAddr, remoteAddr)
	case "sctp":
		return t.sctp(ls)
	default:
		fallthrough
	case "tcp":
		return t.tcp(ls, localAddr, remoteAddr)
	}

	return nil
}

func (t *try) tcp(ls *loopState, localAddr, remoteAddr string) (err error) {
	// Since we specify the source port rather than use an ephemeral port, if
	// the SO_REUSEADDR and SO_REUSEPORT options are not set, when we make
	// another call to this program, the original port is in post-close wait
	// state and bind fails.  The reuse library implements a Dial() that sets
	// these options.
	conn, err := reuse.Dial("tcp", localAddr, remoteAddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	log.Infof("TCP connection established")

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	mtuPair := connectivity.MTUPair{}
	mtuPair.Start, err = utils.ConnMTU(conn.(utils.HasSyscallConn))
	if err != nil {
		log.WithError(err).Error("Failed to read connection MTU")
		return err
	}

	for {
		req := connectivity.NewRequest()
		req.SendSize = t.sendLen
		req.ResponseSize = t.recvLen
		err := encoder.Encode(req)
		if err != nil {
			log.WithError(err).Fatal("Failed to send data")
		}
		log.WithField("message", req).Info("Sent request over TCP")

		if t.sendLen > 0 {
			snd := t.sendLen
			buff := make([]byte, 4096)

			for snd > 0 {
				var err error
				n := 0
				if snd > 4096 {
					n, err = conn.Write(buff)
				} else {
					n, err = conn.Write(buff[:snd])
				}
				snd -= n
				if err != nil {
					log.Errorf("Writing to connection failed. %d bytes too short\n", snd)
					break
				}
			}
		}
		log.Infof("Sent %d extra bytes over TCP", t.sendLen)

		var resp connectivity.Response
		err = decoder.Decode(&resp)
		if err != nil {
			log.WithError(err).Fatal("Failed to read response")
		}
		log.WithField("reply", resp).Info("Got reply")
		if !resp.Request.Equal(req) {
			log.WithField("reply", resp).Fatal("Unexpected response")
		}

		if t.recvLen > 0 {
			rcv := t.recvLen
			buff := make([]byte, 4096)

			r := decoder.Buffered()

			for rcv > 0 {
				n, err := r.Read(buff)
				rcv -= n
				if err == io.EOF {
					break
				}
			}

			for rcv > 0 {
				var err error
				n := 0
				if rcv < 4096 {
					n, err = conn.Read(buff[:rcv])
				} else {
					n, err = conn.Read(buff)
				}
				rcv -= n
				if err != nil {
					log.Fatalf("Reading from connection failed. %d bytes too short\n", rcv)
					break
				}
			}

			log.Infof("Got %d extra bytes\n", t.recvLen)
		}

		j, err := json.Marshal(resp)
		if err != nil {
			log.WithError(err).Fatal("Failed to re-marshal response")
		}
		fmt.Println("RESPONSE=", string(j))

		mtuPair.End, err = utils.ConnMTU(conn.(utils.HasSyscallConn))
		if err != nil {
			log.WithError(err).Error("Failed to read connection MTU")
		}

		mtu, err := json.Marshal(mtuPair)
		if err != nil {
			log.WithError(err).Fatal("Failed to re-marshal MTU")
		}
		fmt.Println("MTU=", string(mtu))

		if !ls.Next() {
			break
		}
	}

	return nil
}

func (t *try) udp(ls *loopState, localAddr, remoteAddr string) {
	// Since we specify the source port rather than use an ephemeral port, if
	// the SO_REUSEADDR and SO_REUSEPORT options are not set, when we make
	// another call to this program, the original port is in post-close wait
	// state and bind fails.  The reuse library implements a Dial() that sets
	// these options.
	conn, err := reuse.Dial("udp", localAddr, remoteAddr)
	log.Infof(`UDP "connection" established`)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	decoder := json.NewDecoder(conn)

	for {
		req := connectivity.NewRequest()
		data, err := json.Marshal(req)
		if err != nil {
			log.WithError(err).Fatal("Failed to marshal data")
		}
		_, err = conn.Write(data)
		if err != nil {
			log.WithError(err).Fatal("Failed to send data")
		}
		log.WithField("message", req).Info("Sent message over UDP")
		var resp connectivity.Response
		err = decoder.Decode(&resp)
		if err != nil {
			log.WithError(err).Fatal("Failed to read response")
		}
		log.WithField("reply", resp).Info("Got reply")
		if !resp.Request.Equal(req) {
			log.WithField("reply", resp).Fatal("Unexpected response")
		}
		j, err := json.Marshal(resp)
		if err != nil {
			log.WithError(err).Fatal("Failed to re-marshal response")
		}
		fmt.Println("RESPONSE=", string(j))
		if !ls.Next() {
			break
		}
	}
}

func (t *try) udpUnConnected(ls *loopState, localAddr, remoteAddr string) {
	conn, err := net.ListenPacket("udp", localAddr)
	if err != nil {
		log.WithError(err).Fatal("Failed to listen UDP")
	}
	remoteAddrResolved, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		log.WithError(err).Fatal("Failed to resolve UDP")
	}
	log.WithFields(log.Fields{
		"addr":               conn.LocalAddr(),
		"remoteAddrResolved": remoteAddrResolved,
	}).Infof("Resolved udp addr")
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	for {
		req := connectivity.NewRequest()
		data, err := json.Marshal(req)
		if err != nil {
			log.WithError(err).Fatal("Failed to marshal data")
		}
		_, err = conn.WriteTo(data, remoteAddrResolved)
		if err != nil {
			log.WithError(err).Fatal("Failed to send data")
		}
		log.WithField("message", req).Infof("Sent message over unconnected UDP to %v", remoteAddr)

		bufIn := make([]byte, 8<<10)
		n, from, err := conn.ReadFrom(bufIn)
		if err != nil {
			log.WithError(err).Fatal("Failed to read from")
		}
		log.Infof("Received %d bytes from %s", n, from)

		if from.String() != remoteAddrResolved.String() {
			log.Fatalf("From address %+v does not match remoteAddr %+v", from, remoteAddrResolved)
		}

		var resp connectivity.Response
		err = json.Unmarshal(bufIn[:n], &resp)
		if err != nil {
			log.WithError(err).Fatal("Failed to read response")
		}
		log.WithField("reply", resp).Info("Got reply")
		if !resp.Request.Equal(req) {
			log.WithField("reply", resp).Fatal("Unexpected response")
		}
		fmt.Println("RESPONSE=", string(bufIn[:n]))
		if !ls.Next() {
			break
		}
	}
}

func (t *try) udpConnectedRecvmsg(ls *loopState, localAddr, remoteAddr string) {
	// Since we specify the source port rather than use an ephemeral port, if
	// the SO_REUSEADDR and SO_REUSEPORT options are not set, when we make
	// another call to this program, the original port is in post-close wait
	// state and bind fails.  The reuse library implements a Dial() that sets
	// these options.
	connB, err := reuse.Dial("udp", localAddr, remoteAddr)
	conn := connB.(*net.UDPConn)
	log.Infof(`UDP "connection" established`)
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	remoteAddrResolved, err := net.ResolveUDPAddr("udp", remoteAddr)
	if err != nil {
		log.WithError(err).Fatal("Failed to resolve UDP")
	}

	for {
		req := connectivity.NewRequest()
		data, err := json.Marshal(req)
		if err != nil {
			log.WithError(err).Fatal("Failed to marshal data")
		}
		_, err = conn.Write(data)
		if err != nil {
			log.WithError(err).Fatal("Failed to send data")
		}
		log.WithField("message", req).Info("Sent message over UDP")
		bufIn := make([]byte, 8<<10)
		n, from, err := conn.ReadFrom(bufIn)
		if err != nil {
			log.WithError(err).Fatal("Failed to read from")
		}
		log.Infof("Received %d bytes from %s", n, from)

		if from.String() != remoteAddrResolved.String() {
			log.Fatalf("From address %+v does not match remoteAddr %+v", from, remoteAddrResolved)
		}

		var resp connectivity.Response
		err = json.Unmarshal(bufIn[:n], &resp)
		if err != nil {
			log.WithError(err).Fatal("Failed to read response")
		}
		log.WithField("reply", resp).Info("Got reply")
		if !resp.Request.Equal(req) {
			log.WithField("reply", resp).Fatal("Unexpected response")
		}
		j, err := json.Marshal(resp)
		if err != nil {
			log.WithError(err).Fatal("Failed to re-marshal response")
		}
		fmt.Println("RESPONSE=", string(j))
		if !ls.Next() {
			break
		}
	}
}

func (t *try) sctp(ls *loopState) error {
	lip, err := net.ResolveIPAddr("ip", "::")
	if err != nil {
		return err
	}
	lport, err := strconv.Atoi(t.sourcePort)
	if err != nil {
		return err
	}
	laddr := &sctp.SCTPAddr{IPAddrs: []net.IPAddr{*lip}, Port: lport}
	rip, err := net.ResolveIPAddr("ip", t.remoteIpAddr)
	if err != nil {
		return err
	}
	rport, err := strconv.Atoi(t.remotePort)
	if err != nil {
		return err
	}
	raddr := &sctp.SCTPAddr{IPAddrs: []net.IPAddr{*rip}, Port: rport}
	// Since we specify the source port rather than use an ephemeral port, if
	// the SO_REUSEADDR and SO_REUSEPORT options are not set, when we make
	// another call to this program, the original port is in post-close wait
	// state and bind fails. The reuse.Dial() does not support SCTP, but the
	// SCTP library has a SocketConfig that accepts a Control function
	// (provided by reuse) that sets these options.
	sCfg := sctp.SocketConfig{Control: reuse.Control}
	conn, err := sCfg.Dial("sctp", laddr, raddr)
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	log.Infof("SCTP connection established")

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	for {
		req := connectivity.NewRequest()
		err := encoder.Encode(req)
		if err != nil {
			log.WithError(err).Fatal("Failed to send data")
		}
		log.WithField("message", req).Info("Sent message over SCTP")
		var resp connectivity.Response
		err = decoder.Decode(&resp)
		if err != nil {
			log.WithError(err).Fatal("Failed to read response")
		}
		log.WithField("reply", resp).Info("Got reply")
		if !resp.Request.Equal(req) {
			log.WithField("reply", resp).Fatal("Unexpected response")
		}
		j, err := json.Marshal(resp)
		if err != nil {
			log.WithError(err).Fatal("Failed to re-marshal response")
		}
		fmt.Println("RESPONSE=", string(j))
		if !ls.Next() {
			break
		}
	}

	return nil
}

type loopState struct {
	sentInitial bool
	loopFile    string
}

func newLoopState(loopFile string) *loopState {
	return &loopState{
		sentInitial: false,
		loopFile:    loopFile,
	}
}

func (l *loopState) Next() bool {
	if l.loopFile == "" {
		return false
	}

	if l.sentInitial {
		// This is after the connection was established in
		// previous iteration, so we wait for the loop file to
		// appear (it should be created by other process). If
		// the file exists, it means that the other process
		// wants us to delete the file, drop the connection
		// and quit.
		if _, err := os.Stat(l.loopFile); err != nil {
			if !os.IsNotExist(err) {
				panic(fmt.Errorf("Failed to stat loop file %s: %v", l.loopFile, err))
			}
		} else {
			if err := os.Remove(l.loopFile); err != nil {
				panic(fmt.Errorf("Could not remove loop file %s: %v", l.loopFile, err))
			}
			return false
		}
	} else {
		// A connection was just established and the initial
		// message was sent so we set the flag to true and
		// delete the loop file, so other process can continue
		// with the appropriate checks
		if err := os.Remove(l.loopFile); err != nil {
			panic(fmt.Errorf("Could not remove loop file %s: %v", l.loopFile, err))
		}
		l.sentInitial = true
	}
	time.Sleep(500 * time.Millisecond)
	return true
}
