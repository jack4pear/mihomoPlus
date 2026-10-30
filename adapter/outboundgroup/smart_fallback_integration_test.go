package outboundgroup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/adapter/provider"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
)

// Route each node's real HTTP probe to an independently controlled endpoint.
type routedProbe struct{ C.Proxy }

func (p *routedProbe) URLTest(ctx context.Context, url string, status utils.IntRanges[uint16]) (uint16, error) {
	return p.Proxy.URLTest(ctx, url+"?node="+p.Name(), status)
}
func (p *routedProbe) URLTestWithResult(ctx context.Context, url string, status utils.IntRanges[uint16]) (uint16, bool, error) {
	return p.Proxy.(*adapter.Proxy).URLTestWithResult(ctx, url+"?node="+p.Name(), status)
}
func (p *routedProbe) AliveForTestUrl(url string) bool {
	return p.Proxy.AliveForTestUrl(url + "?node=" + p.Name())
}

func TestSmartRealHealthChecksAndPriority(t *testing.T) {
	var failA atomic.Bool
	failA.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("node") == "A" && failA.Load() {
			w.WriteHeader(500)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	var proxies []C.Proxy
	for _, name := range []string{"A", "B"} {
		proxies = append(proxies, &routedProbe{adapter.NewProxy(outbound.NewDirectWithOption(outbound.DirectOption{Name: name}))})
	}
	status, _ := utils.NewUnsignedRanges[uint16]("204")
	hc := provider.NewHealthCheck(proxies, server.URL, 100, 1, false, status)
	pd, err := provider.NewCompatibleProvider("test", proxies, hc)
	if err != nil {
		t.Fatal(err)
	}
	defer pd.Close()
	opt := defaultSmartFallbackOption()
	opt.Enabled = true
	opt.FailureThreshold = 1
	opt.Cooldown = []int64{1, 2, 3}
	f, err := NewFallback(GroupCommonOption{Name: "smart", URL: server.URL, ExpectedStatus: "204", Interval: 1, TestTimeout: 100}, FallbackOption{SmartFallback: opt}, adapter.NewProxy(outbound.NewReject()), []P.ProxyProvider{pd})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	pd.HealthCheck()
	if f.Now() != "B" {
		t.Fatalf("expected B, got %s", f.Now())
	}
	if f.Set("A") == nil {
		t.Fatal("manual bypass")
	}
	f.ForceSet("A")
	if f.Now() != "B" {
		t.Fatal("restored selection bypass")
	}
	failA.Store(false)
	for i := 0; i < 3; i++ {
		time.Sleep(1020 * time.Millisecond)
		pd.HealthCheck()
		want := "B"
		if i == 2 {
			want = "A"
		}
		if got := f.Now(); got != want {
			t.Fatalf("recovery %d: want %s, got %s", i, want, got)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = f.Set("B")
				f.Now()
				f.MarshalJSON()
				f.ForceSet("A")
			}
		}()
	}
	wg.Wait()
	f.CloseSmartFallback()
	if _, err := f.DialContext(context.Background(), &C.Metadata{}); !errors.Is(err, ErrSmartFallbackUnavailable) {
		t.Fatal("closed group:", err)
	}
	if _, err := f.ListenPacketContext(context.Background(), &C.Metadata{}); !errors.Is(err, ErrSmartFallbackUnavailable) {
		t.Fatal("closed UDP:", err)
	}
}

func TestSmartParserAndDisabledCompatibility(t *testing.T) {
	direct := adapter.NewProxy(outbound.NewDirectWithOption(outbound.DirectOption{Name: "A"}))
	empty := adapter.NewProxy(outbound.NewReject())
	proxies := map[string]C.Proxy{"A": direct, "COMPATIBLE": empty}
	for _, tc := range []struct {
		name    string
		smart   map[string]any
		wantErr bool
	}{
		{"disabled", map[string]any{"enabled": false}, false},
		{"enabled", map[string]any{"enabled": true}, false},
		{"sticky", map[string]any{"enabled": true, "sticky": true}, true},
		{"fractional", map[string]any{"enabled": true, "window": 1.5}, true},
		{"empty-cooldown", map[string]any{"enabled": true, "cooldown": []int{}}, true},
		{"zero", map[string]any{"enabled": true, "window": 0}, true},
		{"descending", map[string]any{"enabled": true, "cooldown": []int{60, 30}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := map[string]any{"name": "g", "type": "fallback", "proxies": []string{"A"}, "url": "https://probe", "interval": 60, "timeout": 3000, "lazy": false, "smart-fallback": tc.smart}
			providers := map[string]P.ProxyProvider{}
			group, err := ParseProxyGroup(config, proxies, providers, nil, nil)
			for _, pd := range providers {
				defer pd.(interface{ Close() error }).Close()
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if group != nil {
				defer group.Close()
				if group.Now() != "A" {
					t.Fatal(group.Now())
				}
			}
		})
	}
}

func TestSmartEmptyProviderPreservesEmptyFallback(t *testing.T) {
	x := newCircuitFixture()
	empty := adapter.NewProxy(outbound.NewReject())
	f := &Fallback{GroupBase: NewGroupBase(GroupBaseOption{Name: "empty", Type: C.Fallback, EmptyFallback: empty}), smart: x.s}
	if f.findAliveProxy(false) != empty {
		t.Fatal("empty fallback lost")
	}
}
