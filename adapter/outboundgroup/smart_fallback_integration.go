package outboundgroup

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/adapter/provider"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
)

type healthCheckSource interface {
	SubscribeHealthCheck(string, utils.IntRanges[uint16], func(provider.HealthCheckResult)) (func(), time.Duration, time.Duration, error)
}

type unavailableProxy struct{ *outbound.Base }

func (*unavailableProxy) DialContext(context.Context, *C.Metadata) (C.Conn, error) {
	return nil, ErrSmartFallbackUnavailable
}
func (*unavailableProxy) ListenPacketContext(context.Context, *C.Metadata) (C.PacketConn, error) {
	return nil, ErrSmartFallbackUnavailable
}

func (f *Fallback) initSmart(option GroupCommonOption, opt SmartFallbackOption) error {
	if err := opt.validate(); err != nil {
		return err
	}
	if option.Lazy {
		return fmt.Errorf("smart-fallback: group requires lazy=false")
	}
	status, err := utils.NewUnsignedRanges[uint16](f.expectedStatus)
	if err != nil {
		return err
	}
	s := newSmartFallback(f.Name(), opt, 0, 0)
	s.initializing = true
	f.smart = s
	f.unavailable = adapter.NewProxy(&unavailableProxy{outbound.NewBase(outbound.BaseOption{Name: "SMART-FALLBACK-UNAVAILABLE", Type: C.Reject, UDP: true})})
	base := f.GroupBase
	s.current = func() []C.Proxy { return base.GetProxies(false) }
	// Capture only state and base, never f: the finalizer can then release failed parses.
	for _, pd := range f.providers {
		source, ok := pd.(healthCheckSource)
		if !ok {
			f.Close()
			return fmt.Errorf("provider %s does not support health result subscriptions", pd.Name())
		}
		owner := pd
		cancel, interval, timeout, err := source.SubscribeHealthCheck(f.testUrl, status, func(r provider.HealthCheckResult) {
			// A shared node contributes only through its first provider in this group.
			for _, candidate := range base.providers {
				found := false
				for _, p := range candidate.Proxies() {
					if p == r.Proxy {
						found = true
						break
					}
				}
				if found {
					if candidate == owner {
						s.observe(base.GetProxies(false), r)
					}
					return
				}
			}
		})
		if err != nil {
			f.Close()
			return err
		}
		f.unsubscribe = append(f.unsubscribe, cancel)
		s.mu.Lock()
		mismatch := s.interval != 0 && s.interval != interval
		s.interval = interval
		if timeout > s.timeout {
			s.timeout = timeout
		}
		s.mu.Unlock()
		if mismatch {
			f.Close()
			return fmt.Errorf("smart-fallback: providers must use the same health check interval")
		}

	}
	if s.interval == 0 {
		f.Close()
		return fmt.Errorf("smart-fallback: missing continuous health check")
	}
	s.mu.Lock()
	s.initializing = false
	s.mu.Unlock()
	runtime.SetFinalizer(f, (*Fallback).Close)
	return nil
}

func (f *Fallback) selectedName() string {
	f.selectionMu.Lock()
	defer f.selectionMu.Unlock()
	return f.selected
}

func (f *Fallback) smartSet(name string) error {
	proxies := f.GetProxies(false)
	f.selectionMu.Lock()
	defer f.selectionMu.Unlock()
	if err := f.smart.checkSelection(proxies, f.testUrl, name); err != nil {
		return err
	}
	f.selected = name
	return nil
}

// CloseSmartFallback detaches observers on reload without terminating existing connections.
func (f *Fallback) CloseSmartFallback() {
	if f.smart != nil {
		_ = f.Close()
	}
}

func (f *Fallback) Close() error {
	f.closeOnce.Do(func() {
		runtime.SetFinalizer(f, nil)
		if f.smart != nil {
			f.smart.mu.Lock()
			f.smart.closed = true
			f.smart.mu.Unlock()
		}
		for _, cancel := range f.unsubscribe {
			cancel()
		}
	})
	return nil
}
