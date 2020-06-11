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

package intdataplane

import (
	"fmt"
	"net"
	"reflect"
	"regexp"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/projectcalico/felix/bpf/state"
	"github.com/projectcalico/felix/bpf/tc"

	"github.com/projectcalico/felix/idalloc"

	"k8s.io/client-go/kubernetes"

	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"

	"github.com/projectcalico/libcalico-go/lib/health"
	cprometheus "github.com/projectcalico/libcalico-go/lib/prometheus"
	"github.com/projectcalico/libcalico-go/lib/set"

	"github.com/projectcalico/felix/bpf"
	"github.com/projectcalico/felix/bpf/conntrack"
	bpfipsets "github.com/projectcalico/felix/bpf/ipsets"
	"github.com/projectcalico/felix/bpf/nat"
	bpfproxy "github.com/projectcalico/felix/bpf/proxy"
	"github.com/projectcalico/felix/bpf/routes"
	"github.com/projectcalico/felix/ifacemonitor"
	"github.com/projectcalico/felix/ipsets"
	"github.com/projectcalico/felix/iptables"
	"github.com/projectcalico/felix/jitter"
	"github.com/projectcalico/felix/labelindex"
	"github.com/projectcalico/felix/proto"
	"github.com/projectcalico/felix/routetable"
	"github.com/projectcalico/felix/rules"
	"github.com/projectcalico/felix/throttle"
	"github.com/projectcalico/felix/wireguard"
)

const (
	// msgPeekLimit is the maximum number of messages we'll try to grab from the to-dataplane
	// channel before we apply the changes.  Higher values allow us to batch up more work on
	// the channel for greater throughput when we're under load (at cost of higher latency).
	msgPeekLimit = 100

	// Interface name used by kube-proxy to bind service ips.
	KubeIPVSInterface = "kube-ipvs0"
)

var (
	countDataplaneSyncErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "felix_int_dataplane_failures",
		Help: "Number of times dataplane updates failed and will be retried.",
	})
	countMessages = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "felix_int_dataplane_messages",
		Help: "Number dataplane messages by type.",
	}, []string{"type"})
	summaryApplyTime = cprometheus.NewSummary(prometheus.SummaryOpts{
		Name: "felix_int_dataplane_apply_time_seconds",
		Help: "Time in seconds that it took to apply a dataplane update.",
	})
	summaryBatchSize = cprometheus.NewSummary(prometheus.SummaryOpts{
		Name: "felix_int_dataplane_msg_batch_size",
		Help: "Number of messages processed in each batch. Higher values indicate we're " +
			"doing more batching to try to keep up.",
	})
	summaryIfaceBatchSize = cprometheus.NewSummary(prometheus.SummaryOpts{
		Name: "felix_int_dataplane_iface_msg_batch_size",
		Help: "Number of interface state messages processed in each batch. Higher " +
			"values indicate we're doing more batching to try to keep up.",
	})
	summaryAddrBatchSize = cprometheus.NewSummary(prometheus.SummaryOpts{
		Name: "felix_int_dataplane_addr_msg_batch_size",
		Help: "Number of interface address messages processed in each batch. Higher " +
			"values indicate we're doing more batching to try to keep up.",
	})

	processStartTime time.Time
	zeroKey          = wgtypes.Key{}
)

func init() {
	prometheus.MustRegister(countDataplaneSyncErrors)
	prometheus.MustRegister(summaryApplyTime)
	prometheus.MustRegister(countMessages)
	prometheus.MustRegister(summaryBatchSize)
	prometheus.MustRegister(summaryIfaceBatchSize)
	prometheus.MustRegister(summaryAddrBatchSize)
	processStartTime = time.Now()
}

type Config struct {
	Hostname string

	IPv6Enabled          bool
	RuleRendererOverride rules.RuleRenderer
	IPIPMTU              int
	VXLANMTU             int

	MaxIPSetSize int

	IptablesBackend                string
	IPSetsRefreshInterval          time.Duration
	RouteRefreshInterval           time.Duration
	DeviceRouteSourceAddress       net.IP
	DeviceRouteProtocol            int
	RemoveExternalRoutes           bool
	IptablesRefreshInterval        time.Duration
	IptablesPostWriteCheckInterval time.Duration
	IptablesInsertMode             string
	IptablesLockFilePath           string
	IptablesLockTimeout            time.Duration
	IptablesLockProbeInterval      time.Duration
	XDPRefreshInterval             time.Duration

	Wireguard wireguard.Config

	NetlinkTimeout time.Duration

	RulesConfig rules.Config

	IfaceMonitorConfig ifacemonitor.Config

	StatusReportingInterval time.Duration

	ConfigChangedRestartCallback func()

	PostInSyncCallback func()
	HealthAggregator   *health.HealthAggregator
	RouteTableManager  *idalloc.IndexAllocator

	DebugSimulateDataplaneHangAfter time.Duration

	ExternalNodesCidrs []string

	BPFEnabled                         bool
	BPFDisableUnprivileged             bool
	BPFKubeProxyIptablesCleanupEnabled bool
	BPFLogLevel                        string
	BPFDataIfacePattern                *regexp.Regexp
	XDPEnabled                         bool
	XDPAllowGeneric                    bool
	BPFConntrackTimeouts               conntrack.Timeouts
	BPFCgroupV2                        string
	BPFConnTimeLBEnabled               bool
	BPFMapRepin                        bool
	BPFNodePortDSREnabled              bool
	KubeProxyMinSyncPeriod             time.Duration
	KubeProxyEndpointSlicesEnabled     bool

	SidecarAccelerationEnabled bool

	LookPathOverride func(file string) (string, error)

	KubeClientSet *kubernetes.Clientset
}

// InternalDataplane implements an in-process Felix dataplane driver based on iptables
// and ipsets.  It communicates with the datastore-facing part of Felix via the
// Send/RecvMessage methods, which operate on the protobuf-defined API objects.
//
// Architecture
//
// The internal dataplane driver is organised around a main event loop, which handles
// update events from the datastore and dataplane.
//
// Each pass around the main loop has two phases.  In the first phase, updates are fanned
// out to "manager" objects, which calculate the changes that are needed and pass them to
// the dataplane programming layer.  In the second phase, the dataplane layer applies the
// updates in a consistent sequence.  The second phase is skipped until the datastore is
// in sync; this ensures that the first update to the dataplane applies a consistent
// snapshot.
//
// Having the dataplane layer batch updates has several advantages.  It is much more
// efficient to batch updates, since each call to iptables/ipsets has a high fixed cost.
// In addition, it allows for different managers to make updates without having to
// coordinate on their sequencing.
//
// Requirements on the API
//
// The internal dataplane does not do consistency checks on the incoming data (as the
// old Python-based driver used to do).  It expects to be told about dependent resources
// before they are needed and for their lifetime to exceed that of the resources that
// depend on them.  For example, it is important the the datastore layer send an
// IP set create event before it sends a rule that references that IP set.
type InternalDataplane struct {
	toDataplane   chan interface{}
	fromDataplane chan interface{}

	allIptablesTables    []*iptables.Table
	iptablesMangleTables []*iptables.Table
	iptablesNATTables    []*iptables.Table
	iptablesRawTables    []*iptables.Table
	iptablesFilterTables []*iptables.Table
	ipSets               []*ipsets.IPSets

	ipipManager *ipipManager

	wireguardManager *wireguardManager

	ifaceMonitor     *ifacemonitor.InterfaceMonitor
	ifaceUpdates     chan *ifaceUpdate
	ifaceAddrUpdates chan *ifaceAddrsUpdate

	endpointStatusCombiner *endpointStatusCombiner

	allManagers             []Manager
	managersWithRouteTables []ManagerWithRouteTables
	ruleRenderer            rules.RuleRenderer

	interfacePrefixes []string

	// dataplaneNeedsSync is set if the dataplane is dirty in some way, i.e. we need to
	// call apply().
	dataplaneNeedsSync bool
	// forceIPSetsRefresh is set by the IP sets refresh timer to indicate that we should
	// check the IP sets in the dataplane.
	forceIPSetsRefresh bool
	// forceRouteRefresh is set by the route refresh timer to indicate that we should
	// check the routes in the dataplane.
	forceRouteRefresh bool
	// forceXDPRefresh is set by the XDP refresh timer to indicate that we should
	// check the XDP state in the dataplane.
	forceXDPRefresh bool
	// doneFirstApply is set after we finish the first update to the dataplane. It indicates
	// that the dataplane should now be in sync.
	doneFirstApply bool

	reschedTimer *time.Timer
	reschedC     <-chan time.Time

	applyThrottle *throttle.Throttle

	config Config

	debugHangC <-chan time.Time

	xdpState          *xdpState
	sockmapState      *sockmapState
	endpointsSourceV4 endpointsSource
	ipsetsSourceV4    ipsetsSource
	callbacks         *callbacks
}

const (
	healthName     = "int_dataplane"
	healthInterval = 10 * time.Second
)

func NewIntDataplaneDriver(config Config) *InternalDataplane {
	log.WithField("config", config).Info("Creating internal dataplane driver.")
	ruleRenderer := config.RuleRendererOverride
	if ruleRenderer == nil {
		ruleRenderer = rules.NewRenderer(config.RulesConfig)
	}
	epMarkMapper := rules.NewEndpointMarkMapper(
		config.RulesConfig.IptablesMarkEndpoint,
		config.RulesConfig.IptablesMarkNonCaliEndpoint)

	dp := &InternalDataplane{
		toDataplane:       make(chan interface{}, msgPeekLimit),
		fromDataplane:     make(chan interface{}, 100),
		ruleRenderer:      ruleRenderer,
		interfacePrefixes: config.RulesConfig.WorkloadIfacePrefixes,
		ifaceMonitor:      ifacemonitor.New(config.IfaceMonitorConfig),
		ifaceUpdates:      make(chan *ifaceUpdate, 100),
		ifaceAddrUpdates:  make(chan *ifaceAddrsUpdate, 100),
		config:            config,
		applyThrottle:     throttle.New(10),
	}
	dp.applyThrottle.Refill() // Allow the first apply() immediately.
	dp.ifaceMonitor.StateCallback = dp.onIfaceStateChange
	dp.ifaceMonitor.AddrCallback = dp.onIfaceAddrsChange

	backendMode := iptables.DetectBackend(config.LookPathOverride, iptables.NewRealCmd, config.IptablesBackend)

	// Most iptables tables need the same options.
	iptablesOptions := iptables.TableOptions{
		HistoricChainPrefixes: rules.AllHistoricChainNamePrefixes,
		InsertMode:            config.IptablesInsertMode,
		RefreshInterval:       config.IptablesRefreshInterval,
		PostWriteInterval:     config.IptablesPostWriteCheckInterval,
		LockTimeout:           config.IptablesLockTimeout,
		LockProbeInterval:     config.IptablesLockProbeInterval,
		BackendMode:           backendMode,
		LookPathOverride:      config.LookPathOverride,
		OnStillAlive:          dp.reportHealth,
	}

	if config.BPFEnabled && config.BPFKubeProxyIptablesCleanupEnabled {
		// If BPF-mode is enabled, clean up kube-proxy's rules too.
		log.Info("BPF enabled, configuring iptables layer to clean up kube-proxy's rules.")
		iptablesOptions.ExtraCleanupRegexPattern = rules.KubeProxyInsertRuleRegex
		iptablesOptions.HistoricChainPrefixes = append(iptablesOptions.HistoricChainPrefixes, rules.KubeProxyChainPrefixes...)
	}

	// However, the NAT tables need an extra cleanup regex.
	iptablesNATOptions := iptablesOptions
	if iptablesNATOptions.ExtraCleanupRegexPattern == "" {
		iptablesNATOptions.ExtraCleanupRegexPattern = rules.HistoricInsertedNATRuleRegex
	} else {
		iptablesNATOptions.ExtraCleanupRegexPattern += "|" + rules.HistoricInsertedNATRuleRegex
	}

	featureDetector := iptables.NewFeatureDetector()
	iptablesFeatures := featureDetector.GetFeatures()

	var iptablesLock sync.Locker
	if iptablesFeatures.RestoreSupportsLock {
		log.Debug("Calico implementation of iptables lock disabled (because detected version of " +
			"iptables-restore will use its own implementation).")
		iptablesLock = dummyLock{}
	} else if config.IptablesLockTimeout <= 0 {
		log.Debug("Calico implementation of iptables lock disabled (by configuration).")
		iptablesLock = dummyLock{}
	} else {
		// Create the shared iptables lock.  This allows us to block other processes from
		// manipulating iptables while we make our updates.  We use a shared lock because we
		// actually do multiple updates in parallel (but to different tables), which is safe.
		log.WithField("timeout", config.IptablesLockTimeout).Debug(
			"Calico implementation of iptables lock enabled")
		iptablesLock = iptables.NewSharedLock(
			config.IptablesLockFilePath,
			config.IptablesLockTimeout,
			config.IptablesLockProbeInterval,
		)
	}

	mangleTableV4 := iptables.NewTable(
		"mangle",
		4,
		rules.RuleHashPrefix,
		iptablesLock,
		featureDetector,
		iptablesOptions)
	natTableV4 := iptables.NewTable(
		"nat",
		4,
		rules.RuleHashPrefix,
		iptablesLock,
		featureDetector,
		iptablesNATOptions,
	)
	rawTableV4 := iptables.NewTable(
		"raw",
		4,
		rules.RuleHashPrefix,
		iptablesLock,
		featureDetector,
		iptablesOptions)
	filterTableV4 := iptables.NewTable(
		"filter",
		4,
		rules.RuleHashPrefix,
		iptablesLock,
		featureDetector,
		iptablesOptions)
	ipSetsConfigV4 := config.RulesConfig.IPSetConfigV4
	ipSetsV4 := ipsets.NewIPSets(ipSetsConfigV4)
	dp.iptablesNATTables = append(dp.iptablesNATTables, natTableV4)
	dp.iptablesRawTables = append(dp.iptablesRawTables, rawTableV4)
	dp.iptablesMangleTables = append(dp.iptablesMangleTables, mangleTableV4)
	dp.iptablesFilterTables = append(dp.iptablesFilterTables, filterTableV4)
	dp.ipSets = append(dp.ipSets, ipSetsV4)

	if config.RulesConfig.VXLANEnabled {
		routeTableVXLAN := routetable.New([]string{"^vxlan.calico$"}, 4, true, config.NetlinkTimeout,
			config.DeviceRouteSourceAddress, config.DeviceRouteProtocol, true, 0)

		vxlanManager := newVXLANManager(
			ipSetsV4,
			routeTableVXLAN,
			"vxlan.calico",
			config,
		)
		go vxlanManager.KeepVXLANDeviceInSync(config.VXLANMTU, 10*time.Second)
		dp.RegisterManager(vxlanManager)
	} else {
		cleanUpVXLANDevice()
	}

	dp.endpointStatusCombiner = newEndpointStatusCombiner(dp.fromDataplane, config.IPv6Enabled)

	callbacks := newCallbacks()
	dp.callbacks = callbacks
	if !config.BPFEnabled && config.XDPEnabled {
		if err := bpf.SupportsXDP(); err != nil {
			log.WithError(err).Warn("Can't enable XDP acceleration.")
		} else {
			st, err := NewXDPState(config.XDPAllowGeneric)
			if err != nil {
				log.WithError(err).Warn("Can't enable XDP acceleration.")
			} else {
				dp.xdpState = st
				dp.xdpState.PopulateCallbacks(callbacks)
				log.Info("XDP acceleration enabled.")
			}
		}
	} else {
		log.Info("XDP acceleration disabled.")
	}

	// TODO Integrate XDP and BPF infra.
	if !config.BPFEnabled && dp.xdpState == nil {
		xdpState, err := NewXDPState(config.XDPAllowGeneric)
		if err == nil {
			if err := xdpState.WipeXDP(); err != nil {
				log.WithError(err).Warn("Failed to cleanup preexisting XDP state")
			}
		}
		// if we can't create an XDP state it means we couldn't get a working
		// bpffs so there's nothing to clean up
	}

	if config.SidecarAccelerationEnabled {
		if err := bpf.SupportsSockmap(); err != nil {
			log.WithError(err).Warn("Can't enable Sockmap acceleration.")
		} else {
			st, err := NewSockmapState()
			if err != nil {
				log.WithError(err).Warn("Can't enable Sockmap acceleration.")
			} else {
				dp.sockmapState = st
				dp.sockmapState.PopulateCallbacks(callbacks)

				if err := dp.sockmapState.SetupSockmapAcceleration(); err != nil {
					dp.sockmapState = nil
					log.WithError(err).Warn("Failed to set up Sockmap acceleration")
				} else {
					log.Info("Sockmap acceleration enabled.")
				}
			}
		}
	}

	if dp.sockmapState == nil {
		st, err := NewSockmapState()
		if err == nil {
			st.WipeSockmap(bpf.FindInBPFFSOnly)
		}
		// if we can't create a sockmap state it means we couldn't get a working
		// bpffs so there's nothing to clean up
	}

	if !config.BPFEnabled {
		// BPF mode disabled, create the iptables-only managers.
		ipsetsManager := newIPSetsManager(ipSetsV4, config.MaxIPSetSize, callbacks)
		dp.RegisterManager(ipsetsManager)
		dp.ipsetsSourceV4 = ipsetsManager
		// TODO Connect host IP manager to BPF
		dp.RegisterManager(newHostIPManager(
			config.RulesConfig.WorkloadIfacePrefixes,
			rules.IPSetIDThisHostIPs,
			ipSetsV4,
			config.MaxIPSetSize))
		dp.RegisterManager(newPolicyManager(rawTableV4, mangleTableV4, filterTableV4, ruleRenderer, 4, callbacks))

		// Clean up any leftover BPF state.
		err := nat.RemoveConnectTimeLoadBalancer("")
		if err != nil {
			log.WithError(err).Info("Failed to remove BPF connect-time load balancer, ignoring.")
		}
		tc.CleanUpProgramsAndPins()
	}

	if config.BPFEnabled {
		log.Info("BPF enabled, starting BPF endpoint manager and map manager.")
		bpfMapContext := &bpf.MapContext{
			RepinningEnabled: config.BPFMapRepin,
		}
		// Register map managers first since they create the maps that will be used by the endpoint manager.
		// Important that we create the maps before we load a BPF program with TC since we make sure the map
		// metadata name is set whereas TC doesn't set that field.
		ipSetIDAllocator := idalloc.New()
		ipSetsMap := bpfipsets.Map(bpfMapContext)
		dp.RegisterManager(newBPFIPSetManager(ipSetIDAllocator, ipSetsMap))
		bpfRTMgr := newBPFRouteManager(config.Hostname, bpfMapContext)
		dp.RegisterManager(bpfRTMgr)

		// Forwarding into a tunnel seems to fail silently, disable FIB lookup if tunnel is enabled for now.
		fibLookupEnabled := !config.RulesConfig.IPIPEnabled && !config.RulesConfig.VXLANEnabled
		stateMap := state.Map(bpfMapContext)
		err := stateMap.EnsureExists()
		if err != nil {
			log.WithError(err).Panic("Failed to create state BPF map.")
		}
		dp.RegisterManager(newBPFEndpointManager(
			config.BPFLogLevel,
			config.Hostname,
			fibLookupEnabled,
			config.RulesConfig.EndpointToHostAction == "DROP",
			config.BPFDataIfacePattern,
			ipSetIDAllocator,
			config.VXLANMTU,
			config.BPFNodePortDSREnabled,
			ipSetsMap,
			stateMap,
		))

		// Pre-create the NAT maps so that later operations can assume access.
		frontendMap := nat.FrontendMap(bpfMapContext)
		err = frontendMap.EnsureExists()
		if err != nil {
			log.WithError(err).Panic("Failed to create NAT frontend BPF map.")
		}
		backendMap := nat.BackendMap(bpfMapContext)
		err = backendMap.EnsureExists()
		if err != nil {
			log.WithError(err).Panic("Failed to create NAT backend BPF map.")
		}
		backendAffinityMap := nat.AffinityMap(bpfMapContext)
		err = backendAffinityMap.EnsureExists()
		if err != nil {
			log.WithError(err).Panic("Failed to create NAT backend affinity BPF map.")
		}

		routeMap := routes.Map(bpfMapContext)
		err = routeMap.EnsureExists()
		if err != nil {
			log.WithError(err).Panic("Failed to create routes BPF map.")
		}

		ctMap := conntrack.Map(bpfMapContext)
		err = ctMap.EnsureExists()
		if err != nil {
			log.WithError(err).Panic("Failed to create conntrack BPF map.")
		}

		bpfproxyOpts := []bpfproxy.Option{
			bpfproxy.WithMinSyncPeriod(config.KubeProxyMinSyncPeriod),
			bpfproxy.WithConntrackTimeouts(config.BPFConntrackTimeouts),
		}

		if config.KubeProxyEndpointSlicesEnabled {
			bpfproxyOpts = append(bpfproxyOpts, bpfproxy.WithEndpointsSlices())
		}

		if config.BPFNodePortDSREnabled {
			bpfproxyOpts = append(bpfproxyOpts, bpfproxy.WithDSREnabled())
		}

		if config.KubeClientSet != nil {
			// We have a Kubernetes connection, start watching services and populating the NAT maps.
			kp, err := bpfproxy.StartKubeProxy(
				config.KubeClientSet,
				config.Hostname,
				frontendMap,
				backendMap,
				backendAffinityMap,
				ctMap,
				bpfproxyOpts...,
			)
			if err != nil {
				log.WithError(err).Panic("Failed to start kube-proxy.")
			}
			bpfRTMgr.setHostIPUpdatesCallBack(kp.OnHostIPsUpdate)
			bpfRTMgr.setRoutesCallBacks(kp.OnRouteUpdate, kp.OnRouteDelete)
		} else {
			log.Info("BPF enabled but no Kubernetes client available, unable to run kube-proxy module.")
		}

		if config.BPFConnTimeLBEnabled {
			// Activate the connect-time load balancer.
			err = nat.InstallConnectTimeLoadBalancer(frontendMap, backendMap, routeMap, config.BPFCgroupV2, config.BPFLogLevel)
			if err != nil {
				log.WithError(err).Panic("BPFConnTimeLBEnabled but failed to attach connect-time load balancer, bailing out.")
			}
		} else {
			// Deactivate the connect-time load balancer.
			err = nat.RemoveConnectTimeLoadBalancer(config.BPFCgroupV2)
			if err != nil {
				log.WithError(err).Warn("Failed to detach connect-time load balancer. Ignoring.")
			}
		}
	}

	interfaceRegexes := make([]string, len(config.RulesConfig.WorkloadIfacePrefixes))
	for i, r := range config.RulesConfig.WorkloadIfacePrefixes {
		interfaceRegexes[i] = "^" + r + ".*"
	}
	routeTableV4 := routetable.New(interfaceRegexes, 4, false, config.NetlinkTimeout,
		config.DeviceRouteSourceAddress, config.DeviceRouteProtocol, config.RemoveExternalRoutes, 0)

	epManager := newEndpointManager(
		rawTableV4,
		mangleTableV4,
		filterTableV4,
		ruleRenderer,
		routeTableV4,
		4,
		epMarkMapper,
		config.RulesConfig.KubeIPVSSupportEnabled,
		config.RulesConfig.WorkloadIfacePrefixes,
		dp.endpointStatusCombiner.OnEndpointStatusUpdate,
		config.BPFEnabled,
		callbacks)
	dp.RegisterManager(epManager)
	dp.endpointsSourceV4 = epManager
	dp.RegisterManager(newFloatingIPManager(natTableV4, ruleRenderer, 4))
	dp.RegisterManager(newMasqManager(ipSetsV4, natTableV4, ruleRenderer, config.MaxIPSetSize, 4))
	if config.RulesConfig.IPIPEnabled {
		// Add a manger to keep the all-hosts IP set up to date.
		dp.ipipManager = newIPIPManager(ipSetsV4, config.MaxIPSetSize, config.ExternalNodesCidrs)
		dp.RegisterManager(dp.ipipManager) // IPv4-only
	}

	// Add a manager for wireguard configuration. This is added irrespective of whether wireguard is actually enabled
	// because it may need to tidy up some of the routing rules when disabled.
	cryptoRouteTableWireguard := wireguard.New(config.Hostname, &config.Wireguard, config.NetlinkTimeout,
		config.DeviceRouteProtocol, func(publicKey wgtypes.Key) error {
			if publicKey == zeroKey {
				dp.fromDataplane <- &proto.WireguardStatusUpdate{PublicKey: ""}
			} else {
				dp.fromDataplane <- &proto.WireguardStatusUpdate{PublicKey: publicKey.String()}
			}
			return nil
		})
	dp.wireguardManager = newWireguardManager(cryptoRouteTableWireguard)
	dp.RegisterManager(dp.wireguardManager) // IPv4-only

	if config.IPv6Enabled {
		mangleTableV6 := iptables.NewTable(
			"mangle",
			6,
			rules.RuleHashPrefix,
			iptablesLock,
			featureDetector,
			iptablesOptions,
		)
		natTableV6 := iptables.NewTable(
			"nat",
			6,
			rules.RuleHashPrefix,
			iptablesLock,
			featureDetector,
			iptablesNATOptions,
		)
		rawTableV6 := iptables.NewTable(
			"raw",
			6,
			rules.RuleHashPrefix,
			iptablesLock,
			featureDetector,
			iptablesOptions,
		)
		filterTableV6 := iptables.NewTable(
			"filter",
			6,
			rules.RuleHashPrefix,
			iptablesLock,
			featureDetector,
			iptablesOptions,
		)

		ipSetsConfigV6 := config.RulesConfig.IPSetConfigV6
		ipSetsV6 := ipsets.NewIPSets(ipSetsConfigV6)
		dp.ipSets = append(dp.ipSets, ipSetsV6)
		dp.iptablesNATTables = append(dp.iptablesNATTables, natTableV6)
		dp.iptablesRawTables = append(dp.iptablesRawTables, rawTableV6)
		dp.iptablesMangleTables = append(dp.iptablesMangleTables, mangleTableV6)
		dp.iptablesFilterTables = append(dp.iptablesFilterTables, filterTableV6)

		routeTableV6 := routetable.New(
			interfaceRegexes, 6, false, config.NetlinkTimeout,
			config.DeviceRouteSourceAddress, config.DeviceRouteProtocol, config.RemoveExternalRoutes, 0)

		if !config.BPFEnabled {
			dp.RegisterManager(newIPSetsManager(ipSetsV6, config.MaxIPSetSize, callbacks))
			dp.RegisterManager(newHostIPManager(
				config.RulesConfig.WorkloadIfacePrefixes,
				rules.IPSetIDThisHostIPs,
				ipSetsV6,
				config.MaxIPSetSize))
			dp.RegisterManager(newPolicyManager(rawTableV6, mangleTableV6, filterTableV6, ruleRenderer, 6, callbacks))
		}
		dp.RegisterManager(newEndpointManager(
			rawTableV6,
			mangleTableV6,
			filterTableV6,
			ruleRenderer,
			routeTableV6,
			6,
			epMarkMapper,
			config.RulesConfig.KubeIPVSSupportEnabled,
			config.RulesConfig.WorkloadIfacePrefixes,
			dp.endpointStatusCombiner.OnEndpointStatusUpdate,
			config.BPFEnabled,
			callbacks))
		dp.RegisterManager(newFloatingIPManager(natTableV6, ruleRenderer, 6))
		dp.RegisterManager(newMasqManager(ipSetsV6, natTableV6, ruleRenderer, config.MaxIPSetSize, 6))
	}

	dp.allIptablesTables = append(dp.allIptablesTables, dp.iptablesMangleTables...)
	dp.allIptablesTables = append(dp.allIptablesTables, dp.iptablesNATTables...)
	dp.allIptablesTables = append(dp.allIptablesTables, dp.iptablesFilterTables...)
	dp.allIptablesTables = append(dp.allIptablesTables, dp.iptablesRawTables...)

	// Register that we will report liveness and readiness.
	if config.HealthAggregator != nil {
		log.Info("Registering to report health.")
		config.HealthAggregator.RegisterReporter(
			healthName,
			&health.HealthReport{Live: true, Ready: true},
			healthInterval*2,
		)
	}

	if config.DebugSimulateDataplaneHangAfter != 0 {
		log.WithField("delay", config.DebugSimulateDataplaneHangAfter).Warn(
			"Simulating a dataplane hang.")
		dp.debugHangC = time.After(config.DebugSimulateDataplaneHangAfter)
	}

	return dp
}

func cleanUpVXLANDevice() {
	// If VXLAN is not enabled, check to see if there is a VXLAN device and delete it if there is.
	log.Debug("Checking if we need to clean up the VXLAN device")
	link, err := netlink.LinkByName("vxlan.calico")
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			log.Debug("VXLAN disabled and no VXLAN device found")
			return
		}
		log.WithError(err).Warnf("VXLAN disabled and failed to query VXLAN device.  Ignoring.")
		return
	}
	if err = netlink.LinkDel(link); err != nil {
		log.WithError(err).Error("VXLAN disabled and failed to delete unwanted VXLAN device. Ignoring.")
	}
}

type Manager interface {
	// OnUpdate is called for each protobuf message from the datastore.  May either directly
	// send updates to the IPSets and iptables.Table objects (which will queue the updates
	// until the main loop instructs them to act) or (for efficiency) may wait until
	// a call to CompleteDeferredWork() to flush updates to the dataplane.
	OnUpdate(protoBufMsg interface{})
	// Called before the main loop flushes updates to the dataplane to allow for batched
	// work to be completed.
	CompleteDeferredWork() error
}

type ManagerWithRouteTables interface {
	Manager
	GetRouteTableSyncers() []routeTableSyncer
}

func (d *InternalDataplane) routeTableSyncers() []routeTableSyncer {
	var rts []routeTableSyncer
	for _, mrts := range d.managersWithRouteTables {
		rts = append(rts, mrts.GetRouteTableSyncers()...)
	}

	return rts
}

func (d *InternalDataplane) RegisterManager(mgr Manager) {
	switch mgr := mgr.(type) {
	case ManagerWithRouteTables:
		// Used to log the whole manager out here but if we do that then we cause races if the manager has
		// other threads or locks.
		log.WithField("manager", reflect.TypeOf(mgr).Name()).Debug("registering ManagerWithRouteTables")
		d.managersWithRouteTables = append(d.managersWithRouteTables, mgr)
	}
	d.allManagers = append(d.allManagers, mgr)
}

func (d *InternalDataplane) Start() {
	// Do our start-of-day configuration.
	d.doStaticDataplaneConfig()

	// Then, start the worker threads.
	go d.loopUpdatingDataplane()
	go d.loopReportingStatus()
	go d.ifaceMonitor.MonitorInterfaces()
}

// onIfaceStateChange is our interface monitor callback.  It gets called from the monitor's thread.
func (d *InternalDataplane) onIfaceStateChange(ifaceName string, state ifacemonitor.State, ifIndex int) {
	log.WithFields(log.Fields{
		"ifaceName": ifaceName,
		"ifIndex":   ifIndex,
		"state":     state,
	}).Info("Linux interface state changed.")
	d.ifaceUpdates <- &ifaceUpdate{
		Name:  ifaceName,
		State: state,
		Index: ifIndex,
	}
}

type ifaceUpdate struct {
	Name  string
	State ifacemonitor.State
	Index int
}

// Check if current felix ipvs config is correct when felix gets an kube-ipvs0 interface update.
// If KubeIPVSInterface is UP and felix ipvs support is disabled (kube-proxy switched from iptables to ipvs mode),
// or if KubeIPVSInterface is DOWN and felix ipvs support is enabled (kube-proxy switched from ipvs to iptables mode),
// restart felix to pick up correct ipvs support mode.
func (d *InternalDataplane) checkIPVSConfigOnStateUpdate(state ifacemonitor.State) {
	if (!d.config.RulesConfig.KubeIPVSSupportEnabled && state == ifacemonitor.StateUp) ||
		(d.config.RulesConfig.KubeIPVSSupportEnabled && state == ifacemonitor.StateDown) {
		log.WithFields(log.Fields{
			"ipvsIfaceState": state,
			"ipvsSupport":    d.config.RulesConfig.KubeIPVSSupportEnabled,
		}).Info("kube-proxy mode changed. Restart felix.")
		d.config.ConfigChangedRestartCallback()
	}
}

// onIfaceAddrsChange is our interface address monitor callback.  It gets called
// from the monitor's thread.
func (d *InternalDataplane) onIfaceAddrsChange(ifaceName string, addrs set.Set) {
	log.WithFields(log.Fields{
		"ifaceName": ifaceName,
		"addrs":     addrs,
	}).Info("Linux interface addrs changed.")
	d.ifaceAddrUpdates <- &ifaceAddrsUpdate{
		Name:  ifaceName,
		Addrs: addrs,
	}
}

type ifaceAddrsUpdate struct {
	Name  string
	Addrs set.Set
}

func (d *InternalDataplane) SendMessage(msg interface{}) error {
	d.toDataplane <- msg
	return nil
}

func (d *InternalDataplane) RecvMessage() (interface{}, error) {
	return <-d.fromDataplane, nil
}

// doStaticDataplaneConfig sets up the kernel and our static iptables  chains.  Should be called
// once at start of day before starting the main loop.  The actual iptables programming is deferred
// to the main loop.
func (d *InternalDataplane) doStaticDataplaneConfig() {
	// Check/configure global kernel parameters.
	d.configureKernel()

	if d.config.BPFEnabled {
		d.setUpIptablesBPF()
	} else {
		d.setUpIptablesNormal()
	}

	if d.config.RulesConfig.IPIPEnabled {
		log.Info("IPIP enabled, starting thread to keep tunnel configuration in sync.")
		go d.ipipManager.KeepIPIPDeviceInSync(
			d.config.IPIPMTU,
			d.config.RulesConfig.IPIPTunnelAddress,
		)
	} else {
		log.Info("IPIP disabled. Not starting tunnel update thread.")
	}
}

func (d *InternalDataplane) setUpIptablesBPF() {
	// TODO have raw table mark for notrack
	// TODO Any BPF SNAT we need to do

	for _, t := range d.iptablesFilterTables {
		fwdRules := []iptables.Rule{
			{
				// TODO Make "from workload" mark configurable
				Match:  iptables.Match().MarkMatchesWithMask(0xca100000, 0xfff00000),
				Action: iptables.AcceptAction{},
			},
		}
		var inputRules []iptables.Rule
		for _, prefix := range d.config.RulesConfig.WorkloadIfacePrefixes {
			fwdRules = append(fwdRules,
				iptables.Rule{
					Match:   iptables.Match().InInterface(prefix + "+"),
					Action:  iptables.DropAction{},
					Comment: []string{"From workload without BPF ACCEPT mark"},
				})

			if d.config.RulesConfig.EndpointToHostAction == "ACCEPT" {
				// Only need to worry about ACCEPT here.  Drop gets compiled into the BPF program and
				// RETURN would be a no-op since there's nothing to RETURN from.
				inputRules = append(inputRules, iptables.Rule{
					Match:  iptables.Match().InInterface(prefix+"+").MarkMatchesWithMask(0xca100000, 0xfffe0000),
					Action: iptables.AcceptAction{},
				})
			}
			// Catch any workload to host packets that haven't been through the BPF program.
			inputRules = append(inputRules, iptables.Rule{
				Match:  iptables.Match().InInterface(prefix+"+").NotMarkMatchesWithMask(0xca100000, 0xfff00000),
				Action: iptables.DropAction{},
			})
		}
		for _, prefix := range d.config.RulesConfig.WorkloadIfacePrefixes {
			// Make sure iptables rules don't drop packets that we're about to process through BPF.
			fwdRules = append(fwdRules, iptables.Rule{
				Match:   iptables.Match().OutInterface(prefix + "+"),
				Action:  iptables.AcceptAction{},
				Comment: []string{"To workload, BPF will handle."},
			})
		}
		t.SetRuleInsertions("INPUT", inputRules)
		t.SetRuleInsertions("FORWARD", fwdRules)
	}

	for _, t := range d.iptablesNATTables {
		t.UpdateChains(d.ruleRenderer.StaticNATPostroutingChains(t.IPVersion))
		t.SetRuleInsertions("POSTROUTING", []iptables.Rule{{
			Action: iptables.JumpAction{Target: rules.ChainNATPostrouting},
		}})
	}

	for _, t := range d.iptablesRawTables {
		rpfRules := []iptables.Rule{{
			Match:  iptables.Match().MarkMatchesWithMask(0xca140000, 0xffff0000),
			Action: iptables.ReturnAction{},
		}}

		rpfRules = append(rpfRules, rules.RPFilter(t.IPVersion, 0xca100000, 0xfff00000,
			d.config.RulesConfig.OpenStackSpecialCasesEnabled, true)...)

		rpfChain := []*iptables.Chain{{
			Name:  rules.ChainNamePrefix + "RPF",
			Rules: rpfRules,
		}}
		t.UpdateChains(rpfChain)

		rawChains := []*iptables.Chain{{
			Name: rules.ChainRawPrerouting,
			Rules: []iptables.Rule{{
				Action: iptables.JumpAction{Target: rpfChain[0].Name},
			}},
		}}
		t.UpdateChains(rawChains)

		t.SetRuleInsertions("PREROUTING", []iptables.Rule{{
			Action: iptables.JumpAction{Target: rules.ChainRawPrerouting},
		}})
	}
}

func (d *InternalDataplane) setUpIptablesNormal() {
	for _, t := range d.iptablesRawTables {
		rawChains := d.ruleRenderer.StaticRawTableChains(t.IPVersion)
		t.UpdateChains(rawChains)
		t.SetRuleInsertions("PREROUTING", []iptables.Rule{{
			Action: iptables.JumpAction{Target: rules.ChainRawPrerouting},
		}})
		t.SetRuleInsertions("OUTPUT", []iptables.Rule{{
			Action: iptables.JumpAction{Target: rules.ChainRawOutput},
		}})
	}
	for _, t := range d.iptablesFilterTables {
		filterChains := d.ruleRenderer.StaticFilterTableChains(t.IPVersion)
		t.UpdateChains(filterChains)
		t.SetRuleInsertions("FORWARD", []iptables.Rule{{
			Action: iptables.JumpAction{Target: rules.ChainFilterForward},
		}})
		t.SetRuleInsertions("INPUT", []iptables.Rule{{
			Action: iptables.JumpAction{Target: rules.ChainFilterInput},
		}})
		t.SetRuleInsertions("OUTPUT", []iptables.Rule{{
			Action: iptables.JumpAction{Target: rules.ChainFilterOutput},
		}})
	}
	for _, t := range d.iptablesNATTables {
		t.UpdateChains(d.ruleRenderer.StaticNATTableChains(t.IPVersion))
		t.SetRuleInsertions("PREROUTING", []iptables.Rule{{
			Action: iptables.JumpAction{Target: rules.ChainNATPrerouting},
		}})
		t.SetRuleInsertions("POSTROUTING", []iptables.Rule{{
			Action: iptables.JumpAction{Target: rules.ChainNATPostrouting},
		}})
		t.SetRuleInsertions("OUTPUT", []iptables.Rule{{
			Action: iptables.JumpAction{Target: rules.ChainNATOutput},
		}})
	}
	for _, t := range d.iptablesMangleTables {
		t.UpdateChains(d.ruleRenderer.StaticMangleTableChains(t.IPVersion))
		t.SetRuleInsertions("PREROUTING", []iptables.Rule{{
			Action: iptables.JumpAction{Target: rules.ChainManglePrerouting},
		}})
	}
	if d.xdpState != nil {
		if err := d.setXDPFailsafePorts(); err != nil {
			log.Warnf("failed to set XDP failsafe ports, disabling XDP: %v", err)
			if err := d.shutdownXDPCompletely(); err != nil {
				log.Warnf("failed to disable XDP: %v, will proceed anyway.", err)
			}
		}
	}
}

func stringToProtocol(protocol string) (labelindex.IPSetPortProtocol, error) {
	switch protocol {
	case "tcp":
		return labelindex.ProtocolTCP, nil
	case "udp":
		return labelindex.ProtocolUDP, nil
	case "sctp":
		return labelindex.ProtocolSCTP, nil
	}
	return labelindex.ProtocolNone, fmt.Errorf("unknown protocol %q", protocol)
}

func (d *InternalDataplane) setXDPFailsafePorts() error {
	inboundPorts := d.config.RulesConfig.FailsafeInboundHostPorts

	if _, err := d.xdpState.common.bpfLib.NewFailsafeMap(); err != nil {
		return err
	}

	for _, p := range inboundPorts {
		proto, err := stringToProtocol(p.Protocol)
		if err != nil {
			return err
		}

		if err := d.xdpState.common.bpfLib.UpdateFailsafeMap(uint8(proto), p.Port); err != nil {
			return err
		}
	}

	log.Infof("Set XDP failsafe ports: %+v", inboundPorts)
	return nil
}

// shutdownXDPCompletely attempts to disable XDP state.  This could fail in cases where XDP isn't working properly.
func (d *InternalDataplane) shutdownXDPCompletely() error {
	if d.xdpState == nil {
		return nil
	}
	if d.callbacks != nil {
		d.xdpState.DepopulateCallbacks(d.callbacks)
	}
	// spend 1 second attempting to wipe XDP, in case of a hiccup.
	maxTries := 10
	waitInterval := 100 * time.Millisecond
	var err error
	for i := 0; i < maxTries; i++ {
		err = d.xdpState.WipeXDP()
		if err == nil {
			d.xdpState = nil
			return nil
		}
		log.WithError(err).WithField("try", i).Warn("failed to wipe the XDP state")
		time.Sleep(waitInterval)
	}
	return fmt.Errorf("Failed to wipe the XDP state after %v tries over %v seconds: Error %v", maxTries, waitInterval, err)
}

func (d *InternalDataplane) loopUpdatingDataplane() {
	log.Info("Started internal iptables dataplane driver loop")
	healthTicks := time.NewTicker(healthInterval).C
	d.reportHealth()

	// Retry any failed operations every 10s.
	retryTicker := time.NewTicker(10 * time.Second)

	// If configured, start tickers to refresh the IP sets and routing table entries.
	var ipSetsRefreshC <-chan time.Time
	if d.config.IPSetsRefreshInterval > 0 {
		log.WithField("interval", d.config.IptablesRefreshInterval).Info(
			"Will refresh IP sets on timer")
		refreshTicker := jitter.NewTicker(
			d.config.IPSetsRefreshInterval,
			d.config.IPSetsRefreshInterval/10,
		)
		ipSetsRefreshC = refreshTicker.C
	}
	var routeRefreshC <-chan time.Time
	if d.config.RouteRefreshInterval > 0 {
		log.WithField("interval", d.config.RouteRefreshInterval).Info(
			"Will refresh routes on timer")
		refreshTicker := jitter.NewTicker(
			d.config.RouteRefreshInterval,
			d.config.RouteRefreshInterval/10,
		)
		routeRefreshC = refreshTicker.C
	}
	var xdpRefreshC <-chan time.Time
	if d.config.XDPRefreshInterval > 0 && d.xdpState != nil {
		log.WithField("interval", d.config.XDPRefreshInterval).Info(
			"Will refresh XDP on timer")
		refreshTicker := jitter.NewTicker(
			d.config.XDPRefreshInterval,
			d.config.XDPRefreshInterval/10,
		)
		xdpRefreshC = refreshTicker.C
	}

	// Fill the apply throttle leaky bucket.
	throttleC := jitter.NewTicker(100*time.Millisecond, 10*time.Millisecond).C
	beingThrottled := false

	datastoreInSync := false

	processMsgFromCalcGraph := func(msg interface{}) {
		log.WithField("msg", proto.MsgStringer{Msg: msg}).Infof(
			"Received %T update from calculation graph", msg)
		d.recordMsgStat(msg)
		for _, mgr := range d.allManagers {
			mgr.OnUpdate(msg)
		}
		switch msg.(type) {
		case *proto.InSync:
			log.WithField("timeSinceStart", time.Since(processStartTime)).Info(
				"Datastore in sync, flushing the dataplane for the first time...")
			datastoreInSync = true
		}
	}

	processIfaceUpdate := func(ifaceUpdate *ifaceUpdate) {
		log.WithField("msg", ifaceUpdate).Info("Received interface update")
		if ifaceUpdate.Name == KubeIPVSInterface {
			d.checkIPVSConfigOnStateUpdate(ifaceUpdate.State)
			return
		}

		for _, mgr := range d.allManagers {
			mgr.OnUpdate(ifaceUpdate)
		}

		for _, mgr := range d.managersWithRouteTables {
			for _, routeTable := range mgr.GetRouteTableSyncers() {
				routeTable.OnIfaceStateChanged(ifaceUpdate.Name, ifaceUpdate.State)
			}
		}
	}

	processAddrsUpdate := func(ifaceAddrsUpdate *ifaceAddrsUpdate) {
		log.WithField("msg", ifaceAddrsUpdate).Info("Received interface addresses update")
		for _, mgr := range d.allManagers {
			mgr.OnUpdate(ifaceAddrsUpdate)
		}
	}

	for {
		select {
		case msg := <-d.toDataplane:
			// Process the message we received, then opportunistically process any other
			// pending messages.
			batchSize := 1
			processMsgFromCalcGraph(msg)
		msgLoop1:
			for i := 0; i < msgPeekLimit; i++ {
				select {
				case msg := <-d.toDataplane:
					processMsgFromCalcGraph(msg)
					batchSize++
				default:
					// Channel blocked so we must be caught up.
					break msgLoop1
				}
			}
			d.dataplaneNeedsSync = true
			summaryBatchSize.Observe(float64(batchSize))
		case ifaceUpdate := <-d.ifaceUpdates:
			// Process the message we received, then opportunistically process any other
			// pending messages.
			batchSize := 1
			processIfaceUpdate(ifaceUpdate)
		msgLoop2:
			for i := 0; i < msgPeekLimit; i++ {
				select {
				case ifaceUpdate := <-d.ifaceUpdates:
					processIfaceUpdate(ifaceUpdate)
					batchSize++
				default:
					// Channel blocked so we must be caught up.
					break msgLoop2
				}
			}
			d.dataplaneNeedsSync = true
			summaryIfaceBatchSize.Observe(float64(batchSize))
		case ifaceAddrsUpdate := <-d.ifaceAddrUpdates:
			batchSize := 1
			processAddrsUpdate(ifaceAddrsUpdate)
		msgLoop3:
			for i := 0; i < msgPeekLimit; i++ {
				select {
				case ifaceAddrsUpdate := <-d.ifaceAddrUpdates:
					processAddrsUpdate(ifaceAddrsUpdate)
					batchSize++
				default:
					// Channel blocked so we must be caught up.
					break msgLoop3
				}
			}
			summaryAddrBatchSize.Observe(float64(batchSize))
			d.dataplaneNeedsSync = true
		case <-ipSetsRefreshC:
			log.Debug("Refreshing IP sets state")
			d.forceIPSetsRefresh = true
			d.dataplaneNeedsSync = true
		case <-routeRefreshC:
			log.Debug("Refreshing routes")
			d.forceRouteRefresh = true
			d.dataplaneNeedsSync = true
		case <-xdpRefreshC:
			log.Debug("Refreshing XDP")
			d.forceXDPRefresh = true
			d.dataplaneNeedsSync = true
		case <-d.reschedC:
			log.Debug("Reschedule kick received")
			d.dataplaneNeedsSync = true
			// nil out the channel to record that the timer is now inactive.
			d.reschedC = nil
		case <-throttleC:
			d.applyThrottle.Refill()
		case <-healthTicks:
			d.reportHealth()
		case <-retryTicker.C:
		case <-d.debugHangC:
			log.Warning("Debug hang simulation timer popped, hanging the dataplane!!")
			time.Sleep(1 * time.Hour)
			log.Panic("Woke up after 1 hour, something's probably wrong with the test.")
		}

		if datastoreInSync && d.dataplaneNeedsSync {
			// Dataplane is out-of-sync, check if we're throttled.
			if d.applyThrottle.Admit() {
				if beingThrottled && d.applyThrottle.WouldAdmit() {
					log.Info("Dataplane updates no longer throttled")
					beingThrottled = false
				}
				log.Info("Applying dataplane updates")
				applyStart := time.Now()

				// Actually apply the changes to the dataplane.
				d.apply()

				// Record stats.
				applyTime := time.Since(applyStart)
				summaryApplyTime.Observe(applyTime.Seconds())

				if d.dataplaneNeedsSync {
					// Dataplane is still dirty, record an error.
					countDataplaneSyncErrors.Inc()
				}
				log.WithField("msecToApply", applyTime.Seconds()*1000.0).Info(
					"Finished applying updates to dataplane.")

				if !d.doneFirstApply {
					log.WithField(
						"secsSinceStart", time.Since(processStartTime).Seconds(),
					).Info("Completed first update to dataplane.")
					d.doneFirstApply = true
					if d.config.PostInSyncCallback != nil {
						d.config.PostInSyncCallback()
					}
				}
				d.reportHealth()
			} else {
				if !beingThrottled {
					log.Info("Dataplane updates throttled")
					beingThrottled = true
				}
			}
		}
	}
}

func (d *InternalDataplane) configureKernel() {
	// Attempt to modprobe nf_conntrack_proto_sctp.  In some kernels this is a
	// module that needs to be loaded, otherwise all SCTP packets are marked
	// INVALID by conntrack and dropped by Calico's rules.  However, some kernels
	// (confirmed in Ubuntu 19.10's build of 5.3.0-24-generic) include this
	// conntrack without it being a kernel module, and so modprobe will fail.
	// Log result at INFO level for troubleshooting, but otherwise ignore any
	// failed modprobe calls.
	mp := newModProbe(moduleConntrackSCTP, newRealCmd)
	out, err := mp.Exec()
	log.WithError(err).WithField("output", out).Infof("attempted to modprobe %s", moduleConntrackSCTP)

	log.Info("Making sure IPv4 forwarding is enabled.")
	err = writeProcSys("/proc/sys/net/ipv4/ip_forward", "1")
	if err != nil {
		log.WithError(err).Error("Failed to set IPv4 forwarding sysctl")
	}

	if d.config.IPv6Enabled {
		log.Info("Making sure IPv6 forwarding is enabled.")
		err = writeProcSys("/proc/sys/net/ipv6/conf/all/forwarding", "1")
		if err != nil {
			log.WithError(err).Error("Failed to set IPv6 forwarding sysctl")
		}
	}

	if d.config.BPFEnabled && d.config.BPFDisableUnprivileged {
		log.Info("BPF enabled, disabling unprivileged BPF usage.")
		err := writeProcSys("/proc/sys/kernel/unprivileged_bpf_disabled", "1")
		if err != nil {
			log.WithError(err).Error("Failed to set unprivileged_bpf_disabled sysctl")
		}
	}
	if d.config.Wireguard.Enabled {
		// wireguard module is available in linux kernel >= 5.6
		mpwg := newModProbe(moduleWireguard, newRealCmd)
		out, err = mpwg.Exec()
		log.WithError(err).WithField("output", out).Infof("attempted to modprobe %s", moduleWireguard)
	}
}

func (d *InternalDataplane) recordMsgStat(msg interface{}) {
	typeName := reflect.ValueOf(msg).Elem().Type().Name()
	countMessages.WithLabelValues(typeName).Inc()
}

func (d *InternalDataplane) apply() {
	// Update sequencing is important here because iptables rules have dependencies on ipsets.
	// Creating a rule that references an unknown IP set fails, as does deleting an IP set that
	// is in use.

	// Unset the needs-sync flag, we'll set it again if something fails.
	d.dataplaneNeedsSync = false

	// First, give the managers a chance to update IP sets and iptables.
	for _, mgr := range d.allManagers {
		err := mgr.CompleteDeferredWork()
		if err != nil {
			log.WithField("manager", reflect.TypeOf(mgr).Name()).WithError(err).Debug(
				"couldn't complete deferred work for manager, will try again later")
			d.dataplaneNeedsSync = true
		}
		d.reportHealth()
	}

	if d.xdpState != nil {
		if d.forceXDPRefresh {
			// Refresh timer popped.
			d.xdpState.QueueResync()
			d.forceXDPRefresh = false
		}

		var applyXDPError error
		d.xdpState.ProcessPendingDiffState(d.endpointsSourceV4)
		if err := d.applyXDPActions(); err != nil {
			applyXDPError = err
		} else {
			err := d.xdpState.ProcessMemberUpdates()
			d.xdpState.DropPendingDiffState()
			if err != nil {
				log.WithError(err).Warning("Failed to process XDP member updates, will resync later...")
				if err := d.applyXDPActions(); err != nil {
					applyXDPError = err
				}
			}
			d.xdpState.UpdateState()
		}
		if applyXDPError != nil {
			log.WithError(applyXDPError).Info("Applying XDP actions did not succeed, disabling XDP")
			if err := d.shutdownXDPCompletely(); err != nil {
				log.Warnf("failed to disable XDP: %v, will proceed anyway.", err)
			}
		}
	}
	d.reportHealth()

	if d.forceRouteRefresh {
		// Refresh timer popped.
		for _, r := range d.routeTableSyncers() {
			// Queue a resync on the next Apply().
			r.QueueResync()
		}
		d.forceRouteRefresh = false
	}

	if d.forceIPSetsRefresh {
		// Refresh timer popped.
		for _, r := range d.ipSets {
			// Queue a resync on the next Apply().
			r.QueueResync()
		}
		d.forceIPSetsRefresh = false
	}

	// Next, create/update IP sets.  We defer deletions of IP sets until after we update
	// iptables.
	var ipSetsWG sync.WaitGroup
	for _, ipSets := range d.ipSets {
		ipSetsWG.Add(1)
		go func(ipSets *ipsets.IPSets) {
			ipSets.ApplyUpdates()
			d.reportHealth()
			ipSetsWG.Done()
		}(ipSets)
	}

	// Update the routing table in parallel with the other updates.  We'll wait for it to finish
	// before we return.
	var routesWG sync.WaitGroup
	for _, r := range d.routeTableSyncers() {
		routesWG.Add(1)
		go func(r routeTableSyncer) {
			err := r.Apply()
			if err != nil {
				log.Warn("Failed to synchronize routing table, will retry...")
				d.dataplaneNeedsSync = true
			}
			d.reportHealth()
			routesWG.Done()
		}(r)
	}

	// Wait for the IP sets update to finish.  We can't update iptables until it has.
	ipSetsWG.Wait()

	// Update iptables, this should sever any references to now-unused IP sets.
	var reschedDelayMutex sync.Mutex
	var reschedDelay time.Duration
	var iptablesWG sync.WaitGroup
	for _, t := range d.allIptablesTables {
		iptablesWG.Add(1)
		go func(t *iptables.Table) {
			tableReschedAfter := t.Apply()

			reschedDelayMutex.Lock()
			defer reschedDelayMutex.Unlock()
			if tableReschedAfter != 0 && (reschedDelay == 0 || tableReschedAfter < reschedDelay) {
				reschedDelay = tableReschedAfter
			}
			d.reportHealth()
			iptablesWG.Done()
		}(t)
	}
	iptablesWG.Wait()

	// Now clean up any left-over IP sets.
	for _, ipSets := range d.ipSets {
		ipSetsWG.Add(1)
		go func(s *ipsets.IPSets) {
			s.ApplyDeletions()
			d.reportHealth()
			ipSetsWG.Done()
		}(ipSets)
	}
	ipSetsWG.Wait()

	// Wait for the route updates to finish.
	routesWG.Wait()

	// And publish and status updates.
	d.endpointStatusCombiner.Apply()

	// Set up any needed rescheduling kick.
	if d.reschedC != nil {
		// We have an active rescheduling timer, stop it so we can restart it with a
		// different timeout below if it is still needed.
		// This snippet comes from the docs for Timer.Stop().
		if !d.reschedTimer.Stop() {
			// Timer had already popped, drain its channel.
			<-d.reschedC
		}
		// Nil out our copy of the channel to record that the timer is inactive.
		d.reschedC = nil
	}
	if reschedDelay != 0 {
		// We need to reschedule.
		log.WithField("delay", reschedDelay).Debug("Asked to reschedule.")
		if d.reschedTimer == nil {
			// First time, create the timer.
			d.reschedTimer = time.NewTimer(reschedDelay)
		} else {
			// Have an existing timer, reset it.
			d.reschedTimer.Reset(reschedDelay)
		}
		d.reschedC = d.reschedTimer.C
	}
}

func (d *InternalDataplane) applyXDPActions() error {
	var err error = nil
	for i := 0; i < 10; i++ {
		err = d.xdpState.ResyncIfNeeded(d.ipsetsSourceV4)
		if err != nil {
			return err
		}
		if err = d.xdpState.ApplyBPFActions(d.ipsetsSourceV4); err == nil {
			return nil
		} else {
			log.WithError(err).Info("Applying XDP BPF actions did not succeed, will retry with resync...")
		}
	}
	return err
}

func (d *InternalDataplane) loopReportingStatus() {
	log.Info("Started internal status report thread")
	if d.config.StatusReportingInterval <= 0 {
		log.Info("Process status reports disabled")
		return
	}
	// Wait before first report so that we don't check in if we're in a tight cyclic restart.
	time.Sleep(10 * time.Second)
	for {
		uptimeSecs := time.Since(processStartTime).Seconds()
		d.fromDataplane <- &proto.ProcessStatusUpdate{
			IsoTimestamp: time.Now().UTC().Format(time.RFC3339),
			Uptime:       uptimeSecs,
		}
		time.Sleep(d.config.StatusReportingInterval)
	}
}

// iptablesTable is a shim interface for iptables.Table.
type iptablesTable interface {
	UpdateChain(chain *iptables.Chain)
	UpdateChains([]*iptables.Chain)
	RemoveChains([]*iptables.Chain)
	RemoveChainByName(name string)
}

func (d *InternalDataplane) reportHealth() {
	if d.config.HealthAggregator != nil {
		d.config.HealthAggregator.Report(
			healthName,
			&health.HealthReport{Live: true, Ready: d.doneFirstApply},
		)
	}
}

type dummyLock struct{}

func (d dummyLock) Lock() {

}

func (d dummyLock) Unlock() {

}
