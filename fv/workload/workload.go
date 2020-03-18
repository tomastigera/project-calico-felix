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

package workload

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/gomega"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/felix/fv/connectivity"
	"github.com/projectcalico/felix/fv/containers"
	"github.com/projectcalico/felix/fv/infrastructure"
	"github.com/projectcalico/felix/fv/utils"
	api "github.com/projectcalico/libcalico-go/lib/apis/v3"
	"github.com/projectcalico/libcalico-go/lib/backend/k8s/conversion"
	client "github.com/projectcalico/libcalico-go/lib/clientv3"
	"github.com/projectcalico/libcalico-go/lib/options"
)

type Workload struct {
	C                *containers.Container
	Name             string
	InterfaceName    string
	IP               string
	Ports            string
	DefaultPort      string
	runCmd           *exec.Cmd
	outPipe          io.ReadCloser
	errPipe          io.ReadCloser
	namespacePath    string
	WorkloadEndpoint *api.WorkloadEndpoint
	Protocol         string // "tcp" or "udp"
}

var workloadIdx = 0
var sideServIdx = 0
var permConnIdx = 0

func (w *Workload) Stop() {
	if w == nil {
		log.Info("Stop no-op because nil workload")
	} else {
		log.WithField("workload", w).Info("Stop")
		outputBytes, err := utils.Command("docker", "exec", w.C.Name,
			"cat", fmt.Sprintf("/tmp/%v", w.Name)).CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "failed to run docker exec command to get workload pid")
		pid := strings.TrimSpace(string(outputBytes))
		err = utils.Command("docker", "exec", w.C.Name, "kill", pid).Run()
		Expect(err).NotTo(HaveOccurred(), "failed to kill workload")
		_, err = w.runCmd.Process.Wait()
		if err != nil {
			log.WithField("workload", w).Error("failed to wait for process")
		}
		log.WithField("workload", w).Info("Workload now stopped")
	}
}

func Run(c *infrastructure.Felix, name, profile, ip, ports, protocol string) (w *Workload) {
	w, err := run(c, name, profile, ip, ports, protocol)
	if err != nil {
		log.WithError(err).Info("Starting workload failed, retrying")
		w, err = run(c, name, profile, ip, ports, protocol)
	}
	Expect(err).NotTo(HaveOccurred())

	return w
}

func run(c *infrastructure.Felix, name, profile, ip, ports, protocol string) (w *Workload, err error) {
	workloadIdx++
	n := fmt.Sprintf("%s-idx%v", name, workloadIdx)
	interfaceName := conversion.VethNameForWorkload(profile, n)
	if c.IP == ip {
		interfaceName = ""
	}
	// Build unique workload name and struct.
	workloadIdx++
	w = &Workload{
		C:             c.Container,
		Name:          n,
		InterfaceName: interfaceName,
		IP:            ip,
		Ports:         ports,
		Protocol:      protocol,
	}

	// Ensure that the host has the 'test-workload' binary.
	w.C.EnsureBinary("test-workload")

	// Start the workload.
	log.WithField("workload", w).Info("About to run workload")
	var protoArg string
	if protocol == "udp" {
		protoArg = "--udp"
	} else if protocol == "sctp" {
		protoArg = "--sctp"
	}
	w.runCmd = utils.Command("docker", "exec", w.C.Name,
		"sh", "-c",
		fmt.Sprintf("echo $$ > /tmp/%v; exec /test-workload %v '%v' '%v' '%v'",
			w.Name,
			protoArg,
			w.InterfaceName,
			w.IP,
			w.Ports))
	w.outPipe, err = w.runCmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("Getting StdoutPipe failed: %v", err)
	}
	w.errPipe, err = w.runCmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("Getting StderrPipe failed: %v", err)
	}
	err = w.runCmd.Start()
	if err != nil {
		return nil, fmt.Errorf("runCmd Start failed: %v", err)
	}

	// Read the workload's namespace path, which it writes to its standard output.
	stdoutReader := bufio.NewReader(w.outPipe)
	stderrReader := bufio.NewReader(w.errPipe)

	var errDone sync.WaitGroup
	errDone.Add(1)
	go func() {
		defer errDone.Done()
		for {
			line, err := stderrReader.ReadString('\n')
			if err != nil {
				log.WithError(err).Info("End of workload stderr")
				return
			}
			log.Infof("Workload %s stderr: %s", n, strings.TrimSpace(string(line)))
		}
	}()

	namespacePath, err := stdoutReader.ReadString('\n')
	if err != nil {
		// (Only) if we fail here, wait for the stderr to be output before returning.
		defer errDone.Wait()
		if err != nil {
			return nil, fmt.Errorf("Reading from stdout failed: %v", err)
		}
	}

	w.namespacePath = strings.TrimSpace(namespacePath)

	go func() {
		for {
			line, err := stdoutReader.ReadString('\n')
			if err != nil {
				log.WithError(err).Info("End of workload stdout")
				return
			}
			log.Infof("Workload %s stdout: %s", name, strings.TrimSpace(string(line)))
		}
	}()

	log.WithField("workload", w).Info("Workload now running")

	wep := api.NewWorkloadEndpoint()
	wep.Labels = map[string]string{"name": w.Name}
	wep.Spec.Node = w.C.Hostname
	wep.Spec.Orchestrator = "felixfv"
	wep.Spec.Workload = w.Name
	wep.Spec.Endpoint = w.Name
	prefixLen := "32"
	if strings.Contains(w.IP, ":") {
		prefixLen = "128"
	}
	wep.Spec.IPNetworks = []string{w.IP + "/" + prefixLen}
	wep.Spec.InterfaceName = w.InterfaceName
	wep.Spec.Profiles = []string{profile}
	w.WorkloadEndpoint = wep

	return w, nil
}

func (w *Workload) IPNet() string {
	return w.IP + "/32"
}

func (w *Workload) Configure(client client.Interface) {
	wep := w.WorkloadEndpoint
	wep.Namespace = "fv"
	var err error
	w.WorkloadEndpoint, err = client.WorkloadEndpoints().Create(utils.Ctx, w.WorkloadEndpoint, utils.NoOptions)
	Expect(err).NotTo(HaveOccurred(), "Failed to create workload in the calico datastore.")
}

func (w *Workload) RemoveFromDatastore(client client.Interface) {
	_, err := client.WorkloadEndpoints().Delete(utils.Ctx, "fv", w.WorkloadEndpoint.Name, options.DeleteOptions{})
	Expect(err).NotTo(HaveOccurred())
}

func (w *Workload) ConfigureInDatastore(infra infrastructure.DatastoreInfra) {
	wep := w.WorkloadEndpoint
	wep.Namespace = "default"
	var err error
	w.WorkloadEndpoint, err = infra.AddWorkload(wep)
	Expect(err).NotTo(HaveOccurred(), "Failed to add workload")
}

func (w *Workload) NameSelector() string {
	return "name=='" + w.Name + "'"
}

func (w *Workload) SourceName() string {
	return w.Name
}

func (w *Workload) SourceIPs() []string {
	return []string{w.IP}
}

func (w *Workload) CanConnectTo(ip, port, protocol string) *connectivity.Result {
	anyPort := Port{
		Workload: w,
	}
	return anyPort.CanConnectTo(ip, port, protocol)
}

func (w *Workload) CanTransferData(ip, port, protocol string, sendLen, recvLen int) *connectivity.Result {
	anyPort := Port{
		Workload: w,
	}
	return anyPort.CanTransferData(ip, port, protocol, sendLen, recvLen)
}

func (w *Workload) Port(port uint16) *Port {
	return &Port{
		Workload: w,
		Port:     port,
	}
}

func (w *Workload) NamespaceID() string {
	splits := strings.Split(w.namespacePath, "/")
	return splits[len(splits)-1]
}

func (w *Workload) ExecOutput(args ...string) (string, error) {
	args = append([]string{"ip", "netns", "exec", w.NamespaceID()}, args...)
	return w.C.ExecOutput(args...)
}

var (
	rttRegexp = regexp.MustCompile(`rtt=(.*) ms`)
)

func (w *Workload) LatencyTo(ip, port string) (time.Duration, string) {
	if strings.Contains(ip, ":") {
		ip = fmt.Sprintf("[%s]", ip)
	}
	out, err := w.ExecOutput("hping3", "-p", port, "-c", "20", "--fast", "-S", "-n", ip)
	stderr := ""
	if err, ok := err.(*exec.ExitError); ok {
		stderr = string(err.Stderr)
	}
	Expect(err).NotTo(HaveOccurred(), stderr)

	lines := strings.Split(out, "\n")[1:] // Skip header line
	var rttSum time.Duration
	var numBuggyRTTs int
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		matches := rttRegexp.FindStringSubmatch(line)
		Expect(matches).To(HaveLen(2), "Failed to extract RTT from line: "+line)
		rttMsecStr := matches[1]
		rttMsec, err := strconv.ParseFloat(rttMsecStr, 64)
		Expect(err).ToNot(HaveOccurred())
		if rttMsec > 1000 {
			// There's a bug in hping where it occasionally reports RTT+1s instead of RTT.  Work around that
			// but keep track of the number of workarounds and bail out if we see too many.
			rttMsec -= 1000
			numBuggyRTTs++
		}
		rttSum += time.Duration(rttMsec * float64(time.Millisecond))
	}
	Expect(numBuggyRTTs).To(BeNumerically("<", len(lines)/2),
		"hping reported a large number of >1s RTTs; full output:\n"+out)
	meanRtt := rttSum / time.Duration(len(lines))
	return meanRtt, out
}

type SideService struct {
	W       *Workload
	Name    string
	RunCmd  *exec.Cmd
	PidFile string
}

func (s *SideService) Stop() {
	Expect(s.stop()).NotTo(HaveOccurred())
}

func (s *SideService) stop() error {
	log.WithField("SideService", s).Info("Stop")
	output, err := s.W.C.ExecOutput("cat", s.PidFile)
	if err != nil {
		log.WithField("pidfile", s.PidFile).WithError(err).Warn("Failed to get contents of a side service's pidfile")
		return err
	}
	pid := strings.TrimSpace(output)
	err = s.W.C.ExecMayFail("kill", pid)
	if err != nil {
		log.WithField("pid", pid).WithError(err).Warn("Failed to kill a side service")
		return err
	}
	_, err = s.RunCmd.Process.Wait()
	if err != nil {
		log.WithField("side service", s).Error("failed to wait for process")
	}

	log.WithField("SideService", s).Info("Side service now stopped")
	return nil
}

func (w *Workload) StartSideService() *SideService {
	s, err := startSideService(w)
	Expect(err).NotTo(HaveOccurred())
	return s
}

func startSideService(w *Workload) (*SideService, error) {
	// Ensure that the host has the 'test-workload' binary.
	w.C.EnsureBinary("test-workload")
	sideServIdx++
	n := fmt.Sprintf("%s-ss%d", w.Name, sideServIdx)
	pidFile := fmt.Sprintf("/tmp/%s-pid", n)

	testWorkloadShArgs := []string{
		"/test-workload",
	}
	if w.Protocol == "udp" {
		testWorkloadShArgs = append(testWorkloadShArgs, "--udp")
	}
	testWorkloadShArgs = append(testWorkloadShArgs,
		"--sidecar-iptables",
		"--up-lo",
		fmt.Sprintf("'--namespace-path=%s'", w.namespacePath),
		"''", // interface name, not important
		"127.0.0.1",
		"15001",
	)
	pidCmd := fmt.Sprintf("echo $$ >'%s'", pidFile)
	testWorkloadCmd := strings.Join(testWorkloadShArgs, " ")
	dockerWorkloadArgs := []string{
		"docker",
		"exec",
		w.C.Name,
		"sh", "-c",
		fmt.Sprintf("%s; exec %s", pidCmd, testWorkloadCmd),
	}
	runCmd := utils.Command(dockerWorkloadArgs[0], dockerWorkloadArgs[1:]...)
	logName := fmt.Sprintf("side service %s", n)
	if err := utils.LogOutput(runCmd, logName); err != nil {
		return nil, fmt.Errorf("failed to start output logging for %s", logName)
	}
	if err := runCmd.Start(); err != nil {
		return nil, fmt.Errorf("starting /test-workload as a side service failed: %v", err)
	}
	return &SideService{
		W:       w,
		Name:    n,
		RunCmd:  runCmd,
		PidFile: pidFile,
	}, nil
}

type PermanentConnection struct {
	W        *Workload
	LoopFile string
	Name     string
	RunCmd   *exec.Cmd
}

func (pc *PermanentConnection) Stop() {
	Expect(pc.stop()).NotTo(HaveOccurred())
}

func (pc *PermanentConnection) stop() error {
	if err := pc.W.C.ExecMayFail("sh", "-c", fmt.Sprintf("echo > %s", pc.LoopFile)); err != nil {
		log.WithError(err).WithField("loopfile", pc.LoopFile).Warn("Failed to create a loop file to stop the permanent connection")
		return err
	}
	if err := pc.RunCmd.Wait(); err != nil {
		return err
	}
	return nil
}

func (w *Workload) StartPermanentConnection(ip string, port, sourcePort int) *PermanentConnection {
	pc, err := startPermanentConnection(w, ip, port, sourcePort)
	Expect(err).NotTo(HaveOccurred())
	return pc
}

func startPermanentConnection(w *Workload, ip string, port, sourcePort int) (*PermanentConnection, error) {
	// Ensure that the host has the 'test-connection' binary.
	w.C.EnsureBinary("test-connection")
	permConnIdx++
	n := fmt.Sprintf("%s-pc%d", w.Name, permConnIdx)
	loopFile := fmt.Sprintf("/tmp/%s-loop", n)

	err := w.C.ExecMayFail("sh", "-c", fmt.Sprintf("echo > %s", loopFile))
	if err != nil {
		return nil, err
	}

	runCmd := utils.Command(
		"docker",
		"exec",
		w.C.Name,
		"/test-connection",
		w.namespacePath,
		ip,
		fmt.Sprintf("%d", port),
		fmt.Sprintf("--source-port=%d", sourcePort),
		fmt.Sprintf("--protocol=%s", w.Protocol),
		fmt.Sprintf("--loop-with-file=%s", loopFile),
	)
	logName := fmt.Sprintf("permanent connection %s", n)
	if err := utils.LogOutput(runCmd, logName); err != nil {
		return nil, fmt.Errorf("failed to start output logging for %s", logName)
	}
	if err := runCmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start a permanent connection: %v", err)
	}
	Eventually(func() error {
		return w.C.ExecMayFail("stat", loopFile)
	}, 5*time.Second, time.Second).Should(
		HaveOccurred(),
		"Failed to wait for test-connection to be ready, the loop file did not disappear",
	)
	return &PermanentConnection{
		W:        w,
		LoopFile: loopFile,
		Name:     n,
		RunCmd:   runCmd,
	}, nil
}

func (w *Workload) ToMatcher(explicitPort ...uint16) *connectivity.Matcher {
	var port string
	if len(explicitPort) == 1 {
		port = fmt.Sprintf("%d", explicitPort[0])
	} else if w.DefaultPort != "" {
		port = w.DefaultPort
	} else if !strings.Contains(w.Ports, ",") {
		port = w.Ports
	} else {
		panic("Explicit port needed for workload with multiple ports")
	}
	return &connectivity.Matcher{
		IP:         w.IP,
		Port:       port,
		TargetName: fmt.Sprintf("%s on port %s", w.Name, port),
		Protocol:   "tcp",
	}
}

type SpoofedWorkload struct {
	*Workload
	SpoofedSourceIP string
}

func (s *SpoofedWorkload) CanConnectTo(ip, port, protocol string) *connectivity.Result {
	return canConnectTo(s.Workload, ip, port, s.SpoofedSourceIP, "", protocol, 0, 0)
}

type Port struct {
	*Workload
	Port uint16
}

func (p *Port) SourceName() string {
	if p.Port == 0 {
		return p.Name
	}
	return fmt.Sprintf("%s:%d", p.Name, p.Port)
}

func (p *Port) SourceIPs() []string {
	return []string{p.IP}
}

func (p *Port) CanConnectTo(ip, port, protocol string) *connectivity.Result {
	srcPort := strconv.Itoa(int(p.Port))
	return canConnectTo(p.Workload, ip, port, "", srcPort, protocol, 0, 0)
}

func (p *Port) CanTransferData(ip, port, protocol string, sendLen, recvLen int) *connectivity.Result {
	srcPort := strconv.Itoa(int(p.Port))
	return canConnectTo(p.Workload, ip, port, "", srcPort, protocol, sendLen, recvLen)
}

func canConnectTo(w *Workload, ip, port, srcIp, srcPort, protocol string,
	sendLen, recvLen int) *connectivity.Result {

	if protocol == "udp" || protocol == "sctp" {
		// If this is a retry then we may have stale conntrack entries and we don't want those
		// to influence the connectivity check.  UDP lacks a sequence number, so conntrack operates
		// on a simple timer. In the case of SCTP, conntrack appears to match packets even when
		// the conntrack entry is in the CLOSED state.
		if os.Getenv("FELIX_FV_ENABLE_BPF") == "true" {
			w.C.Exec("calico-bpf", "conntrack", "remove", "udp", w.IP, ip)
		} else {
			_ = w.C.ExecMayFail("conntrack", "-D", "-p", protocol, "-s", w.IP, "-d", ip)
		}
	}

	logMsg := "Connection test"

	var opts []connectivity.CheckOption

	if srcIp != "" {
		logMsg += " (spoofed)"
		opts = append(opts, connectivity.WithSourceIP(srcIp))
	}
	if srcPort != "" {
		logMsg += " (with source port)"
		opts = append(opts, connectivity.WithSourcePort(srcPort))
	}

	w.C.EnsureBinary(connectivity.BinaryName)

	return connectivity.Check(w.C.Name, logMsg, ip, port, protocol, sendLen, recvLen, opts...)
}

// ToMatcher implements the connectionTarget interface, allowing this port to be used as
// target.
func (p *Port) ToMatcher(explicitPort ...uint16) *connectivity.Matcher {
	if p.Port == 0 {
		return p.Workload.ToMatcher(explicitPort...)
	}
	return &connectivity.Matcher{
		IP:         p.Workload.IP,
		Port:       fmt.Sprint(p.Port),
		TargetName: fmt.Sprintf("%s on port %d", p.Workload.Name, p.Port),
	}
}
