package provider

import (
	"fmt"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
)

// HealthCheckResult describes one completed provider probe, never a manual URLTest.
type HealthCheckResult struct {
	Proxy               C.Proxy
	URL, ExpectedStatus string
	Started, Finished   time.Time
	Err                 error
}

type healthCheckListener struct {
	mu sync.Mutex
	fn func(HealthCheckResult)
}

func (l *healthCheckListener) emit(r HealthCheckResult) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fn != nil {
		l.fn(r)
	}
}

// SubscribeHealthCheck requires an existing continuous task with identical semantics.
// Cancellation waits for an in-flight callback. Callbacks must not cancel themselves.
func (bp *baseProvider) SubscribeHealthCheck(url string, status utils.IntRanges[uint16], fn func(HealthCheckResult)) (func(), time.Duration, time.Duration, error) {
	hc := bp.healthCheck
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if hc.ctx.Err() != nil || hc.lazy || hc.interval <= 0 || hc.timeout >= hc.interval {
		return nil, 0, 0, fmt.Errorf("provider %s: smart-fallback requires lazy=false, interval>timeout>0", bp.Name())
	}
	actual := hc.expectedStatus
	if url != hc.url {
		opt, ok := hc.extra[url]
		if !ok {
			return nil, 0, 0, fmt.Errorf("provider %s: missing health check URL %s", bp.Name(), url)
		}
		actual = opt.expectedStatus
	}
	if actual.String() != status.String() {
		return nil, 0, 0, fmt.Errorf("provider %s: health check expected-status mismatch", bp.Name())
	}
	listener := &healthCheckListener{fn: func(r HealthCheckResult) {
		if r.URL == url && r.ExpectedStatus == status.String() {
			fn(r)
		}
	}}
	if hc.listeners == nil {
		hc.listeners = make(map[*healthCheckListener]struct{})
	}
	hc.listeners[listener] = struct{}{}
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			hc.mu.Lock()
			delete(hc.listeners, listener)
			hc.mu.Unlock()
			listener.mu.Lock()
			listener.fn = nil
			listener.mu.Unlock()
		})
	}
	return cancel, hc.interval, hc.timeout, nil
}

func (hc *HealthCheck) emit(r HealthCheckResult) {
	hc.mu.Lock()
	listeners := make([]*healthCheckListener, 0, len(hc.listeners))
	for listener := range hc.listeners {
		listeners = append(listeners, listener)
	}
	hc.mu.Unlock()
	for _, listener := range listeners {
		listener.emit(r)
	}
}
