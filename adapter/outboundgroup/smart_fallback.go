package outboundgroup

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter/provider"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
)

var ErrSmartFallbackUnavailable = errors.New("smart-fallback: no eligible healthy proxy")

type SmartFallbackOption struct {
	Enabled          bool    `group:"enabled,omitempty"`
	Window           int64   `group:"window,omitempty"`
	FailureThreshold int     `group:"failure-threshold,omitempty"`
	Cooldown         []int64 `group:"cooldown,omitempty"`
	RecoverSuccess   int     `group:"recover-success,omitempty"`
	ResetAfter       int64   `group:"reset-after,omitempty"`
}

func defaultSmartFallbackOption() SmartFallbackOption {
	return SmartFallbackOption{Window: 600, FailureThreshold: 3, Cooldown: []int64{1800, 3600, 7200}, RecoverSuccess: 3, ResetAfter: 3600}
}

func (o SmartFallbackOption) validate() error {
	if o.FailureThreshold <= 0 || o.RecoverSuccess <= 0 || len(o.Cooldown) == 0 {
		return errors.New("smart-fallback: thresholds must be positive and cooldown nonempty")
	}
	for _, v := range append([]int64{o.Window, o.ResetAfter}, o.Cooldown...) {
		if v <= 0 || v > math.MaxInt64/int64(time.Second) {
			return errors.New("smart-fallback: invalid or overflowing duration")
		}
	}
	for i := 1; i < len(o.Cooldown); i++ {
		if o.Cooldown[i] < o.Cooldown[i-1] {
			return errors.New("smart-fallback: cooldown must be nondecreasing")
		}
	}
	return nil
}

type circuitPhase uint8

const (
	circuitNormal circuitPhase = iota
	circuitCooling
	circuitRecovering
)

type circuitNode struct {
	phase                                                                  circuitPhase
	level                                                                  int // Number of trips since the last stable reset, capped at len(cooldown).
	failures                                                               []time.Time
	until, recoverySince, lastStart, lastFinish, lastRecovery, stableSince time.Time
	successes                                                              int
	observed, healthy                                                      bool
}

type smartFallback struct {
	current           func() []C.Proxy
	initializing      bool
	mu                sync.Mutex
	option            SmartFallbackOption
	now               func() time.Time
	name              string
	nodes             map[C.Proxy]*circuitNode
	interval, timeout time.Duration
	nextEmergency     time.Time
	closed            bool
}

func newSmartFallback(name string, o SmartFallbackOption, interval, timeout time.Duration) *smartFallback {
	return &smartFallback{option: o, now: time.Now, name: name, nodes: make(map[C.Proxy]*circuitNode), interval: interval, timeout: timeout}
}

// syncNodes drops replaced instances, even when the replacement has the same name.
func (s *smartFallback) syncNodes(proxies []C.Proxy) []C.Proxy {
	if s.current != nil {
		proxies = s.current()
	}
	live := make(map[C.Proxy]bool, len(proxies))
	for _, p := range proxies {
		live[p] = true
		if s.nodes[p] == nil {
			s.nodes[p] = &circuitNode{}
		}
	}
	for p := range s.nodes {
		if !live[p] {
			delete(s.nodes, p)
		}
	}
	return proxies
}

func (s *smartFallback) advance(proxies []C.Proxy, now time.Time) {
	hasAvailableState := false
	var earliest C.Proxy
	for _, p := range proxies {
		n := s.nodes[p]
		if n.phase == circuitCooling && !now.Before(n.until) {
			n.phase = circuitRecovering
			n.recoverySince = n.until
			n.successes = 0
			n.lastRecovery = time.Time{}
		}
		if n.phase != circuitCooling {
			hasAvailableState = true
			continue
		}
		if earliest == nil || n.until.Before(s.nodes[earliest].until) {
			earliest = p
		}
	}
	if !hasAvailableState && earliest != nil && !now.Before(s.nextEmergency) {
		n := s.nodes[earliest]
		n.phase = circuitRecovering
		n.recoverySince = now
		n.successes = 0
		n.lastRecovery = time.Time{}
		s.nextEmergency = now.Add(s.interval)
		log.Infoln("[SmartFallback] %s: emergency verification %s", s.name, earliest.Name())
	}
}

func (s *smartFallback) trip(p C.Proxy, n *circuitNode, now time.Time) {
	if n.level < len(s.option.Cooldown) {
		n.level++
	}
	n.phase = circuitCooling
	n.until = now.Add(time.Duration(s.option.Cooldown[n.level-1]) * time.Second)
	n.successes = 0
	n.stableSince = time.Time{}
	n.failures = nil
	s.nextEmergency = now.Add(s.interval)
	log.Infoln("[SmartFallback] %s: cooldown %s level=%d until=%s", s.name, p.Name(), n.level, n.until.Format(time.RFC3339))
}

func (s *smartFallback) observe(proxies []C.Proxy, r provider.HealthCheckResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.initializing {
		return
	}
	proxies = s.syncNodes(proxies)
	n := s.nodes[r.Proxy]
	if n == nil || r.Finished.Before(r.Started) || !r.Started.After(n.lastStart) || r.Started.Before(n.lastFinish) {
		return
	}
	// Never accept a result for a probe begun before its previous accepted probe completed.
	previousStart := n.lastStart
	n.lastStart = r.Started
	n.lastFinish = r.Finished
	n.observed = true
	n.healthy = r.Err == nil
	s.advance(proxies, s.now())
	switch n.phase {
	case circuitCooling:
		return
	case circuitRecovering:
		if r.Started.Before(n.recoverySince) {
			return
		}
		if r.Err != nil {
			s.trip(r.Proxy, n, r.Finished)
			return
		}
		if !n.lastRecovery.IsZero() && r.Started.Sub(n.lastRecovery) < s.interval {
			return
		}
		// Missing/early probes are not successes. Only an actual failure breaks
		// the sequence; scheduler jitter must not make recovery impossible.
		n.lastRecovery = r.Started
		n.successes++
		if n.successes >= s.option.RecoverSuccess {
			n.phase = circuitNormal
			n.failures = nil
			n.stableSince = r.Started
			log.Infoln("[SmartFallback] %s: recovered %s", s.name, r.Proxy.Name())
		}
	case circuitNormal:
		cutoff := r.Finished.Add(-time.Duration(s.option.Window) * time.Second)
		keep := n.failures[:0]
		for _, t := range n.failures {
			if t.After(cutoff) {
				keep = append(keep, t)
			}
		}
		n.failures = keep
		if r.Err != nil {
			n.stableSince = time.Time{}
			n.failures = append(n.failures, r.Finished)
			if len(n.failures) >= s.option.FailureThreshold {
				s.trip(r.Proxy, n, r.Finished)
			}
		} else {
			if n.stableSince.IsZero() || previousStart.IsZero() || r.Started.Sub(previousStart) > s.interval+s.timeout {
				n.stableSince = r.Started
			}
			if n.level > 0 && r.Started.Sub(n.stableSince) >= time.Duration(s.option.ResetAfter)*time.Second {
				n.level = 0
			}
		}
	}
}

func (s *smartFallback) eligible(p C.Proxy, url string) bool {
	n := s.nodes[p]
	return n != nil && n.phase == circuitNormal && (!n.observed || n.healthy) && p.AliveForTestUrl(url)
}

func (s *smartFallback) choose(proxies []C.Proxy, url, selected string) C.Proxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.initializing {
		return nil
	}
	proxies = s.syncNodes(proxies)
	s.advance(proxies, s.now())
	if selected != "" {
		for _, p := range proxies {
			if p.Name() == selected && s.eligible(p, url) {
				return p
			}
		}
	}
	for _, p := range proxies {
		if s.eligible(p, url) {
			return p
		}
	}
	return nil
}

func (s *smartFallback) checkSelection(proxies []C.Proxy, url, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.initializing {
		return ErrSmartFallbackUnavailable
	}
	proxies = s.syncNodes(proxies)
	s.advance(proxies, s.now())
	for _, p := range proxies {
		if p.Name() == name {
			if s.eligible(p, url) {
				return nil
			}
			return fmt.Errorf("%w: %s", ErrSmartFallbackUnavailable, name)
		}
	}
	return errors.New("proxy not exist")
}

// Reject fractional and string values before the upstream weak decoder can truncate them.
func validateSmartNumbers(values map[string]any) error {
	integer := func(v any) bool {
		if v == nil {
			return false
		}
		switch reflect.TypeOf(v).Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return true
		default:
			return false
		}
	}
	for key, value := range values {
		if key == "enabled" {
			continue
		}
		if key == "cooldown" {
			v := reflect.ValueOf(value)
			if !v.IsValid() || (v.Kind() != reflect.Slice && v.Kind() != reflect.Array) {
				return errors.New("smart-fallback: cooldown must be an integer list")
			}
			for i := 0; i < v.Len(); i++ {
				if !integer(v.Index(i).Interface()) {
					return errors.New("smart-fallback: cooldown must contain integers")
				}
			}
		} else if !integer(value) {
			return fmt.Errorf("smart-fallback: %s must be an integer", key)
		}
	}
	return nil
}
