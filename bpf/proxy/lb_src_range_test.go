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

package proxy_test

import (
	"net"

	"github.com/projectcalico/felix/bpf/nat"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sp "k8s.io/kubernetes/pkg/proxy"

	"github.com/projectcalico/felix/bpf/proxy"
	"github.com/projectcalico/felix/ip"
)

func init() {
	logrus.SetOutput(GinkgoWriter)
	logrus.SetLevel(logrus.DebugLevel)
}

var _ = Describe("BPF Load Balancer source range", func() {
	svcs := newMockNATMap()
	eps := newMockNATBackendMap()
	aff := newMockAffinityMap()

	nodeIPs := []net.IP{net.IPv4(192, 168, 0, 1), net.IPv4(10, 123, 0, 1)}
	rt := proxy.NewRTCache()

	s, _ := proxy.NewSyncer(nodeIPs, svcs, eps, aff, rt)

	svcKey := k8sp.ServicePortName{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "test-service",
		},
	}
	state := proxy.DPSyncerState{
		SvcMap: k8sp.ServiceMap{
			svcKey: proxy.NewK8sServicePort(
				net.IPv4(10, 0, 0, 2),
				2222,
				v1.ProtocolTCP,
				proxy.K8sSvcWithExternalIPs([]string{"35.0.0.2"}),
				proxy.K8sSvcWithLBSourceRangeIPs([]string{"35.0.1.2/24", "33.0.1.2/16"}),
			),
		},
		EpsMap: k8sp.EndpointsMap{
			svcKey: []k8sp.Endpoint{&k8sp.BaseEndpointInfo{Endpoint: "10.1.0.1:5555"}},
		},
	}
	makestep := func(step func()) func() {
		return func() {
			defer func() {
				log("svcs = %+v\n", svcs)
				log("eps = %+v\n", eps)
			}()

			step()
		}
	}

	saddr1 := ip.MustParseCIDROrIP("35.0.1.2/24").(ip.V4CIDR)
	saddr2 := ip.MustParseCIDROrIP("33.0.1.2/16").(ip.V4CIDR)
	saddr3 := ip.MustParseCIDROrIP("23.0.1.2/16").(ip.V4CIDR)

	extIP := net.IPv4(35, 0, 0, 2)
	proto := proxy.ProtoV1ToIntPanic(v1.ProtocolTCP)
	keyWithSaddr1 := nat.NewNATKeySrc(extIP, 2222, proto, saddr1)
	keyWithSaddr2 := nat.NewNATKeySrc(extIP, 2222, proto, saddr2)
	keyWithSaddr3 := nat.NewNATKeySrc(extIP, 2222, proto, saddr3)
	keyWithExtIP := nat.NewNATKey(extIP, 2222, proto)
	BlackholeNATVal := nat.NewNATValue(0, nat.BlackHoleCount, 0, 0)

	It("should make the right test transitions", func() {

		By("adding LBSourceRangeIP for existing service", makestep(func() {

			err := s.Apply(state)
			Expect(err).NotTo(HaveOccurred())

			Expect(svcs.m).To(HaveLen(4))

			key := nat.NewNATKeySrc(net.IPv4(10, 0, 0, 2), 2222, proto, saddr1)
			Expect(svcs.m).NotTo(HaveKey(key))

			key = nat.NewNATKeySrc(net.IPv4(10, 0, 0, 2), 2222, proto, saddr2)
			Expect(svcs.m).NotTo(HaveKey(key))

			key = nat.NewNATKey(net.IPv4(10, 0, 0, 2), 2222, proto)
			Expect(svcs.m).To(HaveKey(key))

			Expect(svcs.m).To(HaveKey(keyWithSaddr1))
			Expect(svcs.m).To(HaveKey(keyWithSaddr2))
			Expect(svcs.m).To(HaveKey(keyWithExtIP))

			val, ok := svcs.m[keyWithExtIP]
			Expect(ok).To(BeTrue())
			Expect(val).To(Equal(BlackholeNATVal))

		}))

		By("updating LBSourceRangeIP for existing service", makestep(func() {
			Expect(svcs.m).To(HaveLen(4))
			state.SvcMap[svcKey] = proxy.NewK8sServicePort(
				net.IPv4(10, 0, 0, 2),
				2222,
				v1.ProtocolTCP,
				proxy.K8sSvcWithExternalIPs([]string{"35.0.0.2"}),
				proxy.K8sSvcWithLBSourceRangeIPs([]string{"35.0.1.2/24", "23.0.1.2/16"}),
			)

			err := s.Apply(state)
			Expect(err).NotTo(HaveOccurred())
			Expect(svcs.m).To(HaveLen(4))

			Expect(svcs.m).To(HaveKey(keyWithSaddr1))
			Expect(svcs.m).To(HaveKey(keyWithSaddr3))
			Expect(svcs.m).NotTo(HaveKey(keyWithSaddr2))

		}))

		By("Deleting one LBSourceRangeIP for existing service", makestep(func() {
			state.SvcMap[svcKey] = proxy.NewK8sServicePort(
				net.IPv4(10, 0, 0, 2),
				2222,
				v1.ProtocolTCP,
				proxy.K8sSvcWithExternalIPs([]string{"35.0.0.2"}),
				proxy.K8sSvcWithLBSourceRangeIPs([]string{"35.0.1.2/24"}),
			)

			err := s.Apply(state)
			Expect(err).NotTo(HaveOccurred())
			Expect(svcs.m).To(HaveLen(3))

			Expect(svcs.m).To(HaveKey(keyWithSaddr1))
			Expect(svcs.m).NotTo(HaveKey(keyWithSaddr3))

		}))

		By("Deleting LBSourceRangeIP for existing service", makestep(func() {
			state.SvcMap[svcKey] = proxy.NewK8sServicePort(
				net.IPv4(10, 0, 0, 2),
				2222,
				v1.ProtocolTCP,
				proxy.K8sSvcWithExternalIPs([]string{"35.0.0.2"}),
				proxy.K8sSvcWithLBSourceRangeIPs([]string{}),
			)

			err := s.Apply(state)
			Expect(err).NotTo(HaveOccurred())
			Expect(svcs.m).To(HaveLen(2))

			Expect(svcs.m).NotTo(HaveKey(keyWithSaddr1))
			Expect(svcs.m).NotTo(HaveKey(keyWithSaddr3))

		}))

		By("Adding new entries to the map with different source IPs", makestep(func() {
			state.SvcMap[svcKey] = proxy.NewK8sServicePort(
				net.IPv4(10, 0, 0, 2),
				2222,
				v1.ProtocolTCP,
				proxy.K8sSvcWithExternalIPs([]string{"35.0.0.2", "45.0.1.2"}),
				proxy.K8sSvcWithLBSourceRangeIPs([]string{"33.0.1.2/24", "38.0.1.2/16", "40.0.1.2/32"}),
			)

			err := s.Apply(state)
			Expect(err).NotTo(HaveOccurred())
			Expect(svcs.m).To(HaveLen(9))
			s.Stop()
		}))

		By("Recreate the service with stale entries", makestep(func() {
			state.SvcMap[svcKey] = proxy.NewK8sServicePort(
				net.IPv4(10, 0, 0, 2),
				2222,
				v1.ProtocolTCP,
				proxy.K8sSvcWithExternalIPs([]string{"35.0.0.2"}),
				proxy.K8sSvcWithLBSourceRangeIPs([]string{"35.0.1.2/24"}),
			)
			s, _ = proxy.NewSyncer(nodeIPs, svcs, eps, aff, rt)
			err := s.Apply(state)
			Expect(err).NotTo(HaveOccurred())
			Expect(svcs.m).To(HaveLen(3))
		}))

		By("Recreate the service with stale entries and no lb source IPs", makestep(func() {
			state.SvcMap[svcKey] = proxy.NewK8sServicePort(
				net.IPv4(10, 0, 0, 2),
				2222,
				v1.ProtocolTCP,
				proxy.K8sSvcWithExternalIPs([]string{"35.0.0.2"}),
			)
			s, _ = proxy.NewSyncer(nodeIPs, svcs, eps, aff, rt)
			err := s.Apply(state)
			Expect(err).NotTo(HaveOccurred())
			Expect(svcs.m).To(HaveLen(2))
		}))

		By("deleting the services", makestep(func() {
			delete(state.SvcMap, svcKey)

			err := s.Apply(state)
			Expect(err).NotTo(HaveOccurred())

			Expect(svcs.m).To(HaveLen(0))
			Expect(eps.m).To(HaveLen(0))
		}))

	})
})
