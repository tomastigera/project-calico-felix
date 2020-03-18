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

package containers

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/projectcalico/felix/fv/connectivity"

	. "github.com/onsi/gomega"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/felix/fv/utils"
	"github.com/projectcalico/libcalico-go/lib/set"
)

type Container struct {
	Name           string
	IP             string
	ExtraSourceIPs []string
	IPPrefix       string
	Hostname       string
	runCmd         *exec.Cmd

	mutex         sync.Mutex
	binaries      set.Set
	stdoutWatches []*watch
	stderrWatches []*watch

	logFinished sync.WaitGroup
}

type watch struct {
	regexp *regexp.Regexp
	c      chan struct{}
}

var containerIdx = 0

func (c *Container) Stop() {
	if c == nil {
		log.Info("Stop no-op because nil container")
		return
	}

	logCxt := log.WithField("container", c.Name)
	c.mutex.Lock()
	if c.runCmd == nil {
		logCxt.Info("Stop no-op because container is not running")
		c.mutex.Unlock()
		return
	}
	c.mutex.Unlock()

	logCxt.Info("Stop")

	// Ask docker to stop the container.
	withTimeoutPanic(logCxt, 30*time.Second, c.execDockerStop)
	// Shut down the docker run process (if needed).
	withTimeoutPanic(logCxt, 5*time.Second, func() { c.signalDockerRun(os.Interrupt) })

	// Wait for the container to exit, then escalate to killing it.
	startTime := time.Now()
	for {
		if !c.ListedInDockerPS() {
			// Container has stopped.  Make sure the docker CLI command is dead (it should be already)
			// and wait for its log.
			logCxt.Info("Container stopped (no longer listed in 'docker ps')")
			withTimeoutPanic(logCxt, 5*time.Second, func() { c.signalDockerRun(os.Kill) })
			withTimeoutPanic(logCxt, 10*time.Second, func() { c.logFinished.Wait() })
			return
		}
		if time.Since(startTime) > 2*time.Second {
			logCxt.Info("Container didn't stop, asking docker to kill it")
			// `docker kill` asks the docker daemon to kill the container but, on a
			// resource constrained system, we've seen that fail because the CLI command
			// was blocked so we kill the CLI command too.
			err := exec.Command("docker", "kill", c.Name).Run()
			logCxt.WithError(err).Info("Ran 'docker kill'")
			withTimeoutPanic(logCxt, 5*time.Second, func() { c.signalDockerRun(os.Kill) })
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.WaitNotRunning(60 * time.Second)
	withTimeoutPanic(logCxt, 5*time.Second, func() { c.signalDockerRun(os.Kill) })
	withTimeoutPanic(logCxt, 10*time.Second, func() { c.logFinished.Wait() })

	logCxt.Info("Container stopped")
}

func withTimeoutPanic(logCxt *log.Entry, t time.Duration, f func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()

	select {
	case <-done:
		return
	case <-time.After(t):
		logCxt.Panic("Timeout!")
	}
}

func (c *Container) execDockerStop() {
	logCxt := log.WithField("container", c.Name)
	logCxt.Info("Executing 'docker stop'")
	cmd := exec.Command("docker", "stop", "-t0", c.Name)
	err := cmd.Run()
	if err != nil {
		logCxt.WithError(err).WithField("cmd", cmd).Error("docker stop command failed")
		return
	}
	logCxt.Info("'docker stop' returned success")
}

func (c *Container) signalDockerRun(sig os.Signal) {
	logCxt := log.WithFields(log.Fields{
		"container": c.Name,
		"signal":    sig,
	})
	logCxt.Info("Sending signal to 'docker run' process")
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.runCmd == nil {
		return
	}
	err := c.runCmd.Process.Signal(sig)
	if err != nil {
		logCxt.WithError(err).Error("failed to signal 'docker run' process")
		return
	}
	logCxt.Info("Signalled docker run")
}

type RunOpts struct {
	AutoRemove bool
}

func Run(namePrefix string, opts RunOpts, args ...string) (c *Container) {
	name := UniqueName(namePrefix)
	return RunWithFixedName(name, opts, args...)
}

func UniqueName(namePrefix string) string {
	// Build unique container name and struct.
	containerIdx++
	name := fmt.Sprintf("%v-%d-%d-felixfv", namePrefix, os.Getpid(), containerIdx)
	return name
}

func RunWithFixedName(name string, opts RunOpts, args ...string) (c *Container) {
	c = &Container{Name: name}

	// Prep command to run the container.
	log.WithField("container", c).Info("About to run container")
	runArgs := []string{"run", "--name", c.Name, "--hostname", c.Name}

	if opts.AutoRemove {
		runArgs = append(runArgs, "--rm")
	}

	// Add remaining args
	runArgs = append(runArgs, args...)

	c.runCmd = utils.Command("docker", runArgs...)

	// Get the command's output pipes, so we can merge those into the test's own logging.
	stdout, err := c.runCmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	stderr, err := c.runCmd.StderrPipe()
	Expect(err).NotTo(HaveOccurred())

	// Start the container running.
	err = c.runCmd.Start()
	Expect(err).NotTo(HaveOccurred())

	// Merge container's output into our own logging.
	c.logFinished.Add(2)
	go c.copyOutputToLog("stdout", stdout, &c.logFinished, &c.stdoutWatches)
	go c.copyOutputToLog("stderr", stderr, &c.logFinished, &c.stderrWatches)

	// Note: it might take a long time for the container to start running, e.g. if the image
	// needs to be downloaded.
	c.WaitUntilRunning()

	// Fill in rest of container struct.
	c.IP = c.GetIP()
	c.IPPrefix = c.GetIPPrefix()
	c.Hostname = c.GetHostname()
	c.binaries = set.New()
	log.WithField("container", c).Info("Container now running")
	return
}

func (c *Container) WatchStderrFor(re *regexp.Regexp) chan struct{} {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	log.WithFields(log.Fields{
		"container": c.Name,
		"regex":     re,
	}).Info("Start watching stderr")

	ch := make(chan struct{})
	c.stderrWatches = append(c.stderrWatches, &watch{
		regexp: re,
		c:      ch,
	})
	return ch
}

func (c *Container) WatchStdoutFor(re *regexp.Regexp) chan struct{} {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	log.WithFields(log.Fields{
		"container": c.Name,
		"regex":     re,
	}).Info("Start watching stdout")

	ch := make(chan struct{})
	c.stdoutWatches = append(c.stdoutWatches, &watch{
		regexp: re,
		c:      ch,
	})
	return ch
}

// Start executes "docker start" on a container. Useful when used after Stop()
// to restart a container.
func (c *Container) Start() {
	c.runCmd = utils.Command("docker", "start", "--attach", c.Name)

	stdout, err := c.runCmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	stderr, err := c.runCmd.StderrPipe()
	Expect(err).NotTo(HaveOccurred())

	// Start the container running.
	err = c.runCmd.Start()
	Expect(err).NotTo(HaveOccurred())

	// Merge container's output into our own logging.
	c.logFinished.Add(2)
	go c.copyOutputToLog("stdout", stdout, &c.logFinished, &c.stdoutWatches)
	go c.copyOutputToLog("stderr", stderr, &c.logFinished, nil)

	c.WaitUntilRunning()

	log.WithField("container", c).Info("Container now running")
}

// Remove deletes a container. Should be manually called after a non-auto-removed container
// is stopped.
func (c *Container) Remove() {
	c.runCmd = utils.Command("docker", "rm", "-f", c.Name)
	err := c.runCmd.Start()
	Expect(err).NotTo(HaveOccurred())

	log.WithField("container", c).Info("Removed container.")
}

func (c *Container) copyOutputToLog(streamName string, stream io.Reader, done *sync.WaitGroup, watches *[]*watch) {
	defer done.Done()
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(nil, 10*1024*1024) // Increase maximum buffer size (but don't pre-alloc).
	for scanner.Scan() {
		line := scanner.Text()
		log.Info(c.Name, "[", streamName, "] ", line)

		if watches == nil {
			continue
		}
		c.mutex.Lock()
		for _, w := range *watches {
			if w.c == nil {
				continue
			}
			if !w.regexp.MatchString(line) {
				continue
			}

			log.Info(c.Name, "[", streamName, "] ", "Watch triggered:", w.regexp.String())
			close(w.c)
			w.c = nil
		}
		c.mutex.Unlock()
	}
	logCxt := log.WithFields(log.Fields{
		"name":   c.Name,
		"stream": stream,
	})
	if scanner.Err() != nil {
		logCxt.WithError(scanner.Err()).Error("Non-EOF error reading container stream")
	}
	logCxt.Info("Stream finished")
}

func (c *Container) DockerInspect(format string) string {
	inspectCmd := utils.Command("docker", "inspect",
		"--format="+format,
		c.Name,
	)
	outputBytes, err := inspectCmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred())
	return string(outputBytes)
}

func (c *Container) GetIP() string {
	output := c.DockerInspect("{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}")
	return strings.TrimSpace(output)
}

func (c *Container) GetIPPrefix() string {
	output := c.DockerInspect("{{range .NetworkSettings.Networks}}{{.IPPrefixLen}}{{end}}")
	return strings.TrimSpace(output)
}

func (c *Container) GetHostname() string {
	output := c.DockerInspect("{{.Config.Hostname}}")
	return strings.TrimSpace(output)
}

func (c *Container) GetPIDs(processName string) []int {
	out, err := c.ExecOutput("pgrep", fmt.Sprintf("^%s$", processName))
	if err != nil {
		log.WithError(err).Warn("pgrep failed, assuming no PIDs")
		return nil
	}
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		pid, err := strconv.Atoi(line)
		Expect(err).NotTo(HaveOccurred())
		pids = append(pids, pid)
	}
	return pids
}

type ProcInfo struct {
	PID  int
	PPID int
}

var psRegexp = regexp.MustCompile(`^\s*(\d+)\s+(\d+)\s+(\S+)$`)

func (c *Container) GetProcInfo(processName string) []ProcInfo {
	out, err := c.ExecOutput("ps", "wwxo", "pid,ppid,comm")
	if err != nil {
		log.WithError(err).WithField("out", out).Warn("ps failed, assuming no PIDs")
		return nil
	}
	var pids []ProcInfo
	for _, line := range strings.Split(out, "\n") {
		log.WithField("line", line).Debug("Parsing ps line")
		matches := psRegexp.FindStringSubmatch(line)
		if len(matches) == 0 {
			continue
		}
		name := matches[3]
		if name != processName {
			continue
		}
		pid, err := strconv.Atoi(matches[1])
		if err != nil {
			log.WithError(err).WithField("line", line).Panic("Failed to parse ps output")
		}
		ppid, err := strconv.Atoi(matches[2])
		if err != nil {
			log.WithError(err).WithField("line", line).Panic("Failed to parse ps output")
		}
		pids = append(pids, ProcInfo{PID: pid, PPID: ppid})

	}
	return pids
}

func (c *Container) GetSinglePID(processName string) int {
	// Get the process's PID.  This retry loop ensures that we don't get tripped up if we see multiple
	// PIDs, which can happen transiently when a process restarts.
	start := time.Now()
	for {
		// Get the PID and parent PID of all processes with the right name.
		procs := c.GetProcInfo(processName)
		log.WithField("procs", procs).Debug("Got ProcInfos")
		// Collect all the pids so we can detect forked child processes by their PPID.
		pids := set.New()
		for _, p := range procs {
			pids.Add(p.PID)
		}
		// Filter the procs, ignore any that are children of another proc in the set.
		var filteredProcs []ProcInfo
		for _, p := range procs {
			if pids.Contains(p.PPID) {
				continue
			}
			filteredProcs = append(filteredProcs, p)
		}
		if len(filteredProcs) == 1 {
			// Success, there's one process.
			return filteredProcs[0].PID
		}
		Expect(time.Since(start)).To(BeNumerically("<", time.Second),
			"Timed out waiting for there to be a single PID")
		time.Sleep(50 * time.Millisecond)
	}
}

func (c *Container) WaitUntilRunning() {
	log.Info("Wait for container to be listed in docker ps")

	// Set up so we detect if container startup fails.
	stoppedChan := make(chan struct{})
	go func() {
		defer close(stoppedChan)
		err := c.runCmd.Wait()
		log.WithError(err).WithField("name", c.Name).Info("Container stopped ('docker run' exited)")
		c.mutex.Lock()
		defer c.mutex.Unlock()
		c.runCmd = nil
	}()

	for {
		Expect(stoppedChan).NotTo(BeClosed(), fmt.Sprintf("Container %s failed before being listed in 'docker ps'", c.Name))

		cmd := utils.Command("docker", "ps")
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred())
		if strings.Contains(string(out), c.Name) {
			break
		}
		time.Sleep(1000 * time.Millisecond)
	}
}

func (c *Container) Stopped() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.runCmd == nil
}

func (c *Container) ListedInDockerPS() bool {
	cmd := utils.Command("docker", "ps")
	out, err := cmd.CombinedOutput()
	Expect(err).NotTo(HaveOccurred())
	return strings.Contains(string(out), c.Name)
}

func (c *Container) WaitNotRunning(timeout time.Duration) {
	log.Info("Wait for container not to be listed in docker ps")
	start := time.Now()
	for {
		if !c.ListedInDockerPS() {
			break
		}
		if time.Since(start) > timeout {
			log.Panic("Timed out waiting for container not to be listed.")
		}
		time.Sleep(1000 * time.Millisecond)
	}
}

func (c *Container) EnsureBinary(name string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	logCtx := log.WithField("container", c.Name).WithField("binary", name)
	logCtx.Info("Ensuring binary")
	if !c.binaries.Contains(name) {
		logCtx.Info("Binary not already present")
		err := utils.Command("docker", "cp", "../bin/"+name, c.Name+":/"+name).Run()
		if err != nil {
			log.WithField("name", name).Panic("Failed to run 'docker cp' command")
		}
		c.binaries.Add(name)
	}
}

func (c *Container) CopyFileIntoContainer(hostPath, containerPath string) error {
	cmd := utils.Command("docker", "cp", hostPath, c.Name+":"+containerPath)
	return cmd.Run()
}

func (c *Container) Exec(cmd ...string) {
	log.WithField("container", c.Name).WithField("command", cmd).Info("Running command")
	arg := []string{"exec", c.Name}
	arg = append(arg, cmd...)
	utils.Run("docker", arg...)
}

func (c *Container) ExecMayFail(cmd ...string) error {
	arg := []string{"exec", c.Name}
	arg = append(arg, cmd...)
	return utils.RunMayFail("docker", arg...)
}

func (c *Container) ExecOutput(args ...string) (string, error) {
	arg := []string{"exec", c.Name}
	arg = append(arg, args...)
	cmd := exec.Command("docker", arg...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go c.copyOutputToLog("exec-err", stderr, &wg, nil)
	defer wg.Wait()
	out, err := cmd.Output()
	if err != nil {
		if out == nil {
			return "", err
		}
		return string(out), err
	}
	return string(out), nil
}

func (c *Container) SourceName() string {
	return c.Name
}

func (c *Container) SourceIPs() []string {
	ips := []string{c.IP}
	ips = append(ips, c.ExtraSourceIPs...)
	return ips
}

func (c *Container) checkConnectivity(ip, port, protocol string, sendLen, recvLen int) *connectivity.Response {
	// Ensure that the container has the 'test-connection' binary.
	logCxt := log.WithField("container", c.Name)
	logCxt.Debugf("Entering Container.CanConnectTo(%v,%v,%v)", ip, port, protocol)
	c.EnsureBinary("test-connection")

	// Run 'test-connection' to the target.
	connectionCmd := utils.Command("docker", "exec", c.Name,
		"/test-connection", "--protocol="+protocol,
		fmt.Sprintf("--sendlen=%d", sendLen),
		fmt.Sprintf("--recvlen=%d", recvLen),
		"-", ip, port)
	outPipe, err := connectionCmd.StdoutPipe()
	Expect(err).NotTo(HaveOccurred())
	errPipe, err := connectionCmd.StderrPipe()
	Expect(err).NotTo(HaveOccurred())
	err = connectionCmd.Start()
	Expect(err).NotTo(HaveOccurred())

	var wg sync.WaitGroup
	wg.Add(2)
	var wOut, wErr []byte
	var outErr, errErr error

	go func() {
		defer wg.Done()
		wOut, outErr = ioutil.ReadAll(outPipe)
	}()

	go func() {
		defer wg.Done()
		wErr, errErr = ioutil.ReadAll(errPipe)
	}()

	wg.Wait()
	Expect(outErr).NotTo(HaveOccurred())
	Expect(errErr).NotTo(HaveOccurred())

	err = connectionCmd.Wait()

	logCxt.WithFields(log.Fields{
		"stdout": string(wOut),
		"stderr": string(wErr)}).WithError(err).Info("Connection test")

	if err != nil {
		return nil
	}

	r := regexp.MustCompile(`RESPONSE=(.*)\n`)
	m := r.FindSubmatch(wOut)
	if len(m) > 0 {
		var resp connectivity.Response
		err := json.Unmarshal(m[1], &resp)
		if err != nil {
			logCxt.WithError(err).WithField("output", string(wOut)).Panic("Failed to parse connection check response")
		}
		return &resp
	}
	return nil
}

func (c *Container) CanConnectTo(ip, port, protocol string) *connectivity.Response {
	return c.checkConnectivity(ip, port, protocol, 0, 0)
}

func (c *Container) CanTransferData(ip, port, protocol string, sendLen, recvLen int) *connectivity.Response {
	return c.checkConnectivity(ip, port, protocol, sendLen, recvLen)
}
