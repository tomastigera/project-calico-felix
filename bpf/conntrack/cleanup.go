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

package conntrack

import (
	"net"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/felix/bpf"
)

type Timeouts struct {
	CreationGracePeriod time.Duration

	TCPPreEstablished time.Duration
	TCPEstablished    time.Duration
	TCPFinsSeen       time.Duration
	TCPResetSeen      time.Duration

	UDPLastSeen time.Duration

	ICMPLastSeen time.Duration
}

func DefaultTimeouts() Timeouts {
	return Timeouts{
		CreationGracePeriod: 10 * time.Second,
		TCPPreEstablished:   20 * time.Second,
		TCPEstablished:      time.Hour,
		TCPFinsSeen:         30 * time.Second,
		TCPResetSeen:        40 * time.Second,
		UDPLastSeen:         60 * time.Second,
		ICMPLastSeen:        5 * time.Second,
	}
}

// ScanVerdict represents the set of values returned by EntryScan
type ScanVerdict int

const (
	// ScanVerdictOK means entry is fine and should remain
	ScanVerdictOK = iota
	// ScanVerdictDelete meand entry should be deleted
	ScanVerdictDelete
)

// EntryGet is a function prototype provided to EntryScanner in case it needs to
// evaluate other entries to make a verdict
type EntryGet func(Key) (Value, error)

// EntryScanner is a function prototype to be called on every entry by the scanner
type EntryScanner func(Key, Value, EntryGet) ScanVerdict

// Scanner iterates over a provided conntrack map and call a set of EntryScanner
// functions on each  entry in the order as they were passed to NewScanner. If
// any of the EntryScanner returns ScanVerdictDelete, it deletes the entry, does
// not call any other EntryScanner and continues the iteration.
//
// It provides a delete sae iteration over the conntrack table for multiple
// evaluation functions, to keep their implementation simpler.
type Scanner struct {
	ctMap    bpf.Map
	scanners []EntryScanner
}

// NewScanner returns a scanner for the given conntrack map and the set of
// EntryScanner. They are executed in the provided order on each entry.
func NewScanner(ctMap bpf.Map, scanners ...EntryScanner) *Scanner {
	return &Scanner{
		ctMap:    ctMap,
		scanners: scanners,
	}
}

// Scan executes a scanning iteration
func (s *Scanner) Scan() {
	err := s.ctMap.Iter(func(k, v []byte) {
		ctKey := KeyFromBytes(k)
		ctVal := ValueFromBytes(v)
		log.WithFields(log.Fields{
			"key":   ctKey,
			"entry": ctVal,
		}).Debug("Examining conntrack entry")

		for _, scanner := range s.scanners {
			if verdict := scanner(ctKey, ctVal, s.get); verdict == ScanVerdictDelete {
				err := s.ctMap.Delete(k)
				log.WithError(err).Debug("Deletion result")
				if err != nil && !bpf.IsNotExists(err) {
					log.WithError(err).WithField("key", ctKey).Warn("Failed to delete conntrack entry")
				}
				break // the entry is no more
			}
		}
	})

	if err != nil {
		log.WithError(err).Warn("Failed to iterate over conntrack map")
	}
}

func (s *Scanner) get(k Key) (Value, error) {
	v, err := s.ctMap.Get(k.AsBytes())

	if err != nil {
		return Value{}, err
	}

	return ValueFromBytes(v), nil
}

type LivenessScanner struct {
	timeouts Timeouts
	dsr      bool
	NowNanos func() int64
}

func NewLivenessScanner(timeouts Timeouts, dsr bool) *LivenessScanner {
	return &LivenessScanner{
		timeouts: timeouts,
		dsr:      dsr,
		NowNanos: bpf.KTimeNanos,
	}
}

func (l *LivenessScanner) ScanEntry(ctKey Key, ctVal Value, get EntryGet) ScanVerdict {
	now := l.NowNanos()

	switch ctVal.Type() {
	case TypeNATForward:
		// Look up the reverse entry, where we do the book-keeping.
		revEntry, err := get(ctVal.ReverseNATKey())
		if err != nil && bpf.IsNotExists(err) {
			// Forward entry exists but no reverse entry. We might have come across the reverse
			// entry first and removed it. It is useless on its own, so delete it now.
			log.Info("Found a forward NAT conntrack entry with no reverse entry, removing...")
			return ScanVerdictDelete
		} else if err != nil {
			log.WithError(err).Warn("Failed to look up conntrack entry.")
			return ScanVerdictOK
		}
		if reason, expired := l.EntryExpired(now, ctKey.Proto(), revEntry); expired {
			log.WithField("reason", reason).Debug("Deleting expired conntrack forward-NAT entry")
			return ScanVerdictDelete
			// do not delete the reverse entry yet to avoid breaking the iterating
			// over the map.  We must not delete other than the current key. We remove
			// it once we come across it again.
		}
	case TypeNATReverse:
		if reason, expired := l.EntryExpired(now, ctKey.Proto(), ctVal); expired {
			log.WithField("reason", reason).Debug("Deleting expired conntrack reverse-NAT entry")
			return ScanVerdictDelete
		}
	case TypeNormal:
		if reason, expired := l.EntryExpired(now, ctKey.Proto(), ctVal); expired {
			log.WithField("reason", reason).Debug("Deleting expired normal conntrack entry")
			return ScanVerdictDelete
		}
	default:
		log.WithField("type", ctVal.Type()).Warn("Unknown conntrack entry type!")
	}

	return ScanVerdictOK
}

func (l *LivenessScanner) EntryExpired(nowNanos int64, proto uint8, entry Value) (reason string, expired bool) {
	sinceCreation := time.Duration(nowNanos - entry.Created())
	if sinceCreation < l.timeouts.CreationGracePeriod {
		log.Debug("Conntrack entry in creation grace period. Ignoring.")
		return
	}
	age := time.Duration(nowNanos - entry.LastSeen())
	switch proto {
	case ProtoTCP:
		dsr := entry.IsForwardDSR()
		data := entry.Data()
		rstSeen := data.RSTSeen()
		if rstSeen && age > l.timeouts.TCPResetSeen {
			return "RST seen", true
		}
		finsSeen := (dsr && data.FINsSeenDSR()) || data.FINsSeen()
		if finsSeen && age > l.timeouts.TCPFinsSeen {
			// Both legs have been finished, tear down.
			return "FINs seen", true
		}
		if data.Established() || dsr {
			if age > l.timeouts.TCPEstablished {
				return "no traffic on established flow for too long", true
			}
		} else {
			if age > l.timeouts.TCPPreEstablished {
				return "no traffic on pre-established flow for too long", true
			}
		}
		return "", false
	case ProtoICMP:
		if age > l.timeouts.ICMPLastSeen {
			return "no traffic on ICMP flow for too long", true
		}
	default:
		// FIXME separate timeouts for non-UDP IP traffic?
		if age > l.timeouts.UDPLastSeen {
			return "no traffic on UDP flow for too long", true
		}
	}
	return "", false
}

// NATChecker returns true a given combination of frontend-backend exists
type NATChecker func(frontIP net.IP, frontPort uint16, backIP net.IP, backPort uint16, proto uint8) bool

// NewStaleNATScanner returns an EntryScanner that checks if entries have
// exisitng NAT entries using the provided NATChecker and if not, it deletes
// them.
func NewStaleNATScanner(frontendHasBackend NATChecker) EntryScanner {
	return func(k Key, v Value, get EntryGet) ScanVerdict {
		switch v.Type() {
		case TypeNormal:
			// skip non-NAT entry

		case TypeNATReverse:
			proto := k.Proto()
			ipA := k.AddrA()
			ipB := k.AddrB()

			portA := k.PortA()
			portB := k.PortB()

			svcIP := v.OrigIP()
			svcPort := v.OrigPort()

			// We cannot tell which leg is EP and which is the client, we must
			// try both. If there is a record for one of them, it is still most
			// likely an active entry.
			if !frontendHasBackend(svcIP, svcPort, ipA, portA, proto) &&
				!frontendHasBackend(svcIP, svcPort, ipB, portB, proto) {
				log.WithField("key", k).Debugf("TypeNATReverse is stale")
				return ScanVerdictDelete
			}
			log.WithField("key", k).Debugf("TypeNATReverse still active")

		case TypeNATForward:
			proto := k.Proto()
			kA := k.AddrA()
			kB := k.AddrB()
			revKey := v.ReverseNATKey()
			revA := revKey.AddrA()
			revB := revKey.AddrB()

			var (
				svcIP, epIP     net.IP
				svcPort, epPort uint16
			)

			// Because client IP/Port are both in fwd key and rev key, we can
			// can tell which one it is and thus determine exactly meaning of
			// the other values.
			if kA.Equal(revA) {
				epIP = revB
				epPort = revKey.PortB()
				svcIP = kB
				svcPort = k.PortB()
			} else if kA.Equal(revB) {
				epIP = revA
				epPort = revKey.PortA()
				svcIP = kA
				svcPort = k.PortA()
			} else {
				log.WithFields(log.Fields{"key": k, "value": v}).Error("Mismatch between key and rev key")
			}

			if !frontendHasBackend(svcIP, svcPort, epIP, epPort, proto) {
				log.WithField("key", k).Debugf("TypeNATForward is stale")
				return ScanVerdictDelete
			}
			log.WithField("key", k).Debugf("TypeNATForward still active")

		default:
			log.WithField("conntrack.Value.Type()", v.Type()).Warn("Unknown type")
		}

		return ScanVerdictOK
	}
}
