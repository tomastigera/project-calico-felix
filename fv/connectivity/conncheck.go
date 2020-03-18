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

package connectivity

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/types"
	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/felix/fv/utils"
	"github.com/projectcalico/libcalico-go/lib/set"

	uuid "github.com/satori/go.uuid"
)

// ConnectivityChecker records a set of connectivity expectations and supports calculating the
// actual state of the connectivity between the given workloads.  It is expected to be used like so:
//
//     var cc = &conncheck.ConnectivityChecker{}
//     cc.ExpectNone(w[2], w[0], 1234)
//     cc.ExpectSome(w[1], w[0], 5678)
//     cc.CheckConnectivity()
//
type Checker struct {
	ReverseDirection bool
	Protocol         string // "tcp" or "udp"
	expectations     []Expectation
	CheckSNAT        bool
}

func (c *Checker) ExpectSome(from ConnectionSource, to ConnectionTarget, explicitPort ...uint16) {
	c.expect(true, from, to, explicitPort)
}

func (c *Checker) ExpectSNAT(from ConnectionSource, srcIP string, to ConnectionTarget, explicitPort ...uint16) {
	c.CheckSNAT = true
	c.expect(true, from, to, explicitPort, ExpectWithSrcIPs(srcIP))
}

func (c *Checker) ExpectNone(from ConnectionSource, to ConnectionTarget, explicitPort ...uint16) {
	c.expect(false, from, to, explicitPort)
}

// ExpectDataTransfer check if sendLen can be send to the dest and whether
// recvLen of data can be received from the dest reliably
func (c *Checker) ExpectDataTransfer(from TransferSource, to ConnectionTarget,
	ports []uint16, opts ...ExpectationOption) {
	c.expect(true, from, to, ports, opts...)
}

func (c *Checker) expect(connectivity bool, from ConnectionSource, to ConnectionTarget,
	explicitPort []uint16, opts ...ExpectationOption) {

	UnactivatedCheckers.Add(c)
	if c.ReverseDirection {
		from, to = to.(ConnectionSource), from.(ConnectionTarget)
	}

	e := Expectation{
		From:     from,
		To:       to.ToMatcher(explicitPort...),
		Expected: connectivity,
	}

	if connectivity {
		// we expect the from.SourceIPs() by default
		e.ExpSrcIPs = from.SourceIPs()
	}

	for _, option := range opts {
		option(&e)
	}

	c.expectations = append(c.expectations, e)
}

func (c *Checker) ResetExpectations() {
	c.expectations = nil
	c.CheckSNAT = false
}

// ActualConnectivity calculates the current connectivity for all the expected paths.  It returns a
// slice containing one response for each attempted check (or nil if the check failed) along with
// a same-length slice containing a pretty-printed description of the check and its result.
func (c *Checker) ActualConnectivity() ([]*Result, []string) {
	UnactivatedCheckers.Discard(c)
	var wg sync.WaitGroup
	results := make([]*Result, len(c.expectations))
	pretty := make([]string, len(c.expectations))
	for i, exp := range c.expectations {
		wg.Add(1)
		go func(i int, exp Expectation) {
			defer ginkgo.GinkgoRecover()
			defer wg.Done()
			p := "tcp"
			if c.Protocol != "" {
				p = c.Protocol
			}

			var res *Result
			if exp.sendLen > 0 || exp.recvLen > 0 {
				res = exp.From.(TransferSource).
					CanTransferData(exp.To.IP, exp.To.Port, p, exp.sendLen, exp.recvLen)
			} else {
				res = exp.From.CanConnectTo(exp.To.IP, exp.To.Port, p)
			}
			pretty[i] = fmt.Sprintf("%s -> %s = %v", exp.From.SourceName(), exp.To.TargetName, res != nil)
			if res != nil {
				if c.CheckSNAT {
					srcIP := strings.Split(res.Response().SourceAddr, ":")[0]
					pretty[i] += " (from " + srcIP + ")"
				}
				pretty[i] += fmt.Sprintf(" client MTU %d -> %d", res.clientMTU.Start, res.clientMTU.Start)
			}
			results[i] = res
		}(i, exp)
	}
	wg.Wait()
	log.Debug("Connectivity", results)
	return results, pretty
}

// ExpectedConnectivityPretty returns one string per recorded expectation in order, encoding the expected
// connectivity in similar format used by ActualConnectivity().
func (c *Checker) ExpectedConnectivityPretty() []string {
	result := make([]string, len(c.expectations))
	for i, exp := range c.expectations {
		result[i] = fmt.Sprintf("%s -> %s = %v", exp.From.SourceName(), exp.To.TargetName, exp.Expected)
		if exp.Expected {
			if c.CheckSNAT {
				result[i] += " (from " + strings.Join(exp.ExpSrcIPs, "|") + ")"
			}
			if exp.clientMTUStart != 0 || exp.clientMTUEnd != 0 {
				result[i] += fmt.Sprintf(" client MTU %d -> %d", exp.clientMTUStart, exp.clientMTUEnd)
			}
		}
	}
	return result
}

var defaultConnectivityTimeout = 10 * time.Second

func (c *Checker) CheckConnectivityOffset(offset int, optionalDescription ...interface{}) {
	c.CheckConnectivityWithTimeoutOffset(offset+2, defaultConnectivityTimeout, optionalDescription...)
}

func (c *Checker) CheckConnectivity(optionalDescription ...interface{}) {
	c.CheckConnectivityWithTimeoutOffset(2, defaultConnectivityTimeout, optionalDescription...)
}

func (c *Checker) CheckConnectivityWithTimeout(timeout time.Duration, optionalDescription ...interface{}) {
	gomega.Expect(timeout).To(gomega.BeNumerically(">", 100*time.Millisecond),
		"Very low timeout, did you mean to multiply by time.<Unit>?")
	if len(optionalDescription) > 0 {
		gomega.Expect(optionalDescription[0]).NotTo(gomega.BeAssignableToTypeOf(time.Second),
			"Unexpected time.Duration passed for description")
	}
	c.CheckConnectivityWithTimeoutOffset(2, timeout, optionalDescription...)
}

func (c *Checker) CheckConnectivityWithTimeoutOffset(callerSkip int, timeout time.Duration, optionalDescription ...interface{}) {
	var expConnectivity []string
	start := time.Now()

	// Track the number of attempts. If the first connectivity check fails, we want to
	// do at least one retry before we time out.  That covers the case where the first
	// connectivity check takes longer than the timeout.
	completedAttempts := 0
	var actualConn []*Result
	var actualConnPretty []string
	for time.Since(start) < timeout || completedAttempts < 2 {
		actualConn, actualConnPretty = c.ActualConnectivity()
		failed := false
		expConnectivity = c.ExpectedConnectivityPretty()
		for i := range c.expectations {
			exp := c.expectations[i]
			act := actualConn[i]
			if !exp.Matches(act, c.CheckSNAT) {
				failed = true
				actualConnPretty[i] += " <---- WRONG"
				expConnectivity[i] += " <---- EXPECTED"
			}
		}
		if !failed {
			// Success!
			return
		}
		completedAttempts++
	}

	message := fmt.Sprintf(
		"Connectivity was incorrect:\n\nExpected\n    %s\nto match\n    %s",
		strings.Join(actualConnPretty, "\n    "),
		strings.Join(expConnectivity, "\n    "),
	)
	ginkgo.Fail(message, callerSkip)
}

func NewRequest() Request {
	return Request{
		Timestamp: time.Now(),
		ID:        uuid.NewV4().String(),
	}
}

type Request struct {
	Timestamp    time.Time
	ID           string
	SendSize     int
	ResponseSize int
}

func (req Request) Equal(oth Request) bool {
	return req.ID == oth.ID && req.Timestamp.Equal(oth.Timestamp)
}

type Response struct {
	Timestamp time.Time

	SourceAddr string
	ServerAddr string

	Request Request
}

func (r *Response) SourceIP() string {
	return strings.Split(r.SourceAddr, ":")[0]
}

// Result of the Check()
type Result struct {
	response  *Response
	clientMTU MTUPair
}

// Response returns the server response associated with the result
func (r *Result) Response() *Response {
	return r.response
}

// MTUPair is a pair of MTU value recorded before and after data were transfered
type MTUPair struct {
	Start int
	End   int
}

type ConnectionTarget interface {
	ToMatcher(explicitPort ...uint16) *Matcher
}

type TargetIP string // Just so we can define methods on it...

func (s TargetIP) ToMatcher(explicitPort ...uint16) *Matcher {
	if len(explicitPort) != 1 {
		panic("Explicit port needed with IP as a connectivity target")
	}
	port := fmt.Sprintf("%d", explicitPort[0])
	return &Matcher{
		IP:         string(s),
		Port:       port,
		TargetName: string(s) + ":" + port,
		Protocol:   "tcp",
	}
}

func HaveConnectivityTo(target ConnectionTarget, explicitPort ...uint16) types.GomegaMatcher {
	return target.ToMatcher(explicitPort...)
}

type Matcher struct {
	IP, Port, TargetName, Protocol string
}

type ConnectionSource interface {
	CanConnectTo(ip, port, protocol string) *Result
	SourceName() string
	SourceIPs() []string
}

// TransferSource can connect and also can transfer data to/from
type TransferSource interface {
	ConnectionSource
	CanTransferData(ip, port, protocol string, sendLen, recvLen int) *Result
}

func (m *Matcher) Match(actual interface{}) (success bool, err error) {
	success = actual.(ConnectionSource).CanConnectTo(m.IP, m.Port, m.Protocol) != nil
	return
}

func (m *Matcher) FailureMessage(actual interface{}) (message string) {
	src := actual.(ConnectionSource)
	message = fmt.Sprintf("Expected %v\n\t%+v\nto have connectivity to %v\n\t%v:%v\nbut it does not", src.SourceName(), src, m.TargetName, m.IP, m.Port)
	return
}

func (m *Matcher) NegatedFailureMessage(actual interface{}) (message string) {
	src := actual.(ConnectionSource)
	message = fmt.Sprintf("Expected %v\n\t%+v\nnot to have connectivity to %v\n\t%v:%v\nbut it does", src.SourceName(), src, m.TargetName, m.IP, m.Port)
	return
}

type ExpectationOption func(e *Expectation)

func ExpectWithSrcIPs(ips ...string) ExpectationOption {
	return func(e *Expectation) {
		e.ExpSrcIPs = ips
	}
}

// ExpectWithSendLen asserts how much additional data on top of the original
// requests should be sent with success
func ExpectWithSendLen(l int) ExpectationOption {
	return func(e *Expectation) {
		e.sendLen = l
	}
}

// ExpectWithRecvLen asserts how much additional data on top of the original
// response should be received with success
func ExpectWithRecvLen(l int) ExpectationOption {
	return func(e *Expectation) {
		e.recvLen = l
	}
}

// ExpectWithClientAdjustedMTU asserts that the connection MTU should change
// during the transfer
func ExpectWithClientAdjustedMTU(from, to int) ExpectationOption {
	return func(e *Expectation) {
		e.clientMTUStart = from
		e.clientMTUEnd = to
	}
}

type Expectation struct {
	From      ConnectionSource // Workload or Container
	To        *Matcher         // Workload or IP, + port
	Expected  bool
	ExpSrcIPs []string

	sendLen int
	recvLen int

	clientMTUStart int
	clientMTUEnd   int
}

func (e Expectation) Matches(res *Result, checkSNAT bool) bool {
	if res == nil {
		return false
	}

	response := res.Response()
	if e.Expected {
		if response == nil {
			return false
		}
		if checkSNAT {
			match := false
			for _, src := range e.ExpSrcIPs {
				if src == response.SourceIP() {
					match = true
					break
				}
			}
			if !match {
				return false
			}
		}

		if e.clientMTUStart != 0 && e.clientMTUStart != res.clientMTU.Start {
			return false
		}
		if e.clientMTUEnd != 0 && e.clientMTUEnd != res.clientMTU.End {
			return false
		}
	} else {
		if response != nil {
			return false
		}
	}
	return true
}

var UnactivatedCheckers = set.New()

// CheckOption is the option format for Check()
type CheckOption func(cmd *CheckCmd)

// CheckCmd is exported solely for the sake of CheckOption and should not be use
// on its own
type CheckCmd struct {
	ip       string
	port     string
	protocol string

	ipSource   string
	portSource string

	sendLen int
	recvLen int
}

// BinaryName is the name of the binry that the connectivity Check() executes
const BinaryName = "test-connection"

// Run executes the check command
func (cmd *CheckCmd) run(cName string, logMsg string) *Result {
	// Ensure that the container has the 'test-connection' binary.
	logCxt := log.WithField("container", cName)
	logCxt.Debugf("Entering connectivity.Check(%v,%v,%v,%v,%v)",
		cmd.ip, cmd.port, cmd.protocol, cmd.sendLen, cmd.recvLen)

	args := []string{"exec", cName,
		"/test-connection", "--protocol=" + cmd.protocol,
		fmt.Sprintf("--sendlen=%d", cmd.sendLen),
		fmt.Sprintf("--recvlen=%d", cmd.recvLen),
		"-", cmd.ip, cmd.port,
	}

	if cmd.ipSource != "" {
		args = append(args, fmt.Sprintf("--source-ip=%s", cmd.ipSource))
	}

	if cmd.portSource != "" {
		args = append(args, fmt.Sprintf("--source-port=%s", cmd.portSource))
	}

	// Run 'test-connection' to the target.
	connectionCmd := utils.Command("docker", args...)

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
		"stderr": string(wErr)}).WithError(err).Info(logMsg)

	if err != nil {
		return nil
	}

	res := &Result{
		response: &Response{},
	}

	r := regexp.MustCompile(`RESPONSE=(.*)\n`)
	m := r.FindSubmatch(wOut)
	if len(m) == 0 {
		logCxt.WithField("output", string(wOut)).Panic("Missing response")
		return nil
	}

	err = json.Unmarshal(m[1], res.response)
	if err != nil {
		logCxt.WithError(err).WithField("output", string(wOut)).
			Panic("Failed to parse connection check response")
		return nil
	}

	r = regexp.MustCompile(`MTU=(.*)\n`)
	m = r.FindSubmatch(wOut)
	if len(m) == 0 {
		logCxt.WithField("output", string(wOut)).Panic("Missing MTU")
		return nil
	}

	err = json.Unmarshal(m[1], &res.clientMTU)
	if err != nil {
		logCxt.WithError(err).WithField("output", string(wOut)).
			Panic("Failed to parse client MTU")
		return nil
	}

	return res
}

// WithSourceIP tell the check what source IP to use
func WithSourceIP(ip string) CheckOption {
	return func(c *CheckCmd) {
		c.ipSource = ip
	}
}

// WithSourcePort tell the check what source port to use
func WithSourcePort(port string) CheckOption {
	return func(c *CheckCmd) {
		c.portSource = port
	}
}

// Check executes the connectivity check
func Check(cName, logMsg, ip, port, protocol string, sendLen, recvLen int, opts ...CheckOption) *Result {

	cmd := CheckCmd{
		ip:       ip,
		port:     port,
		protocol: protocol,
		sendLen:  sendLen,
		recvLen:  recvLen,
	}

	for _, opt := range opts {
		opt(&cmd)
	}

	return cmd.run(cName, logMsg)
}
