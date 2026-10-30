package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
)

func TestHealthResultSubscribeNetworkAndManualIsolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	p := adapter.NewProxy(outbound.NewDirect())
	status, _ := utils.NewUnsignedRanges[uint16]("204")
	hc := NewHealthCheck([]C.Proxy{p}, server.URL, 500, 60, false, status)
	defer hc.close()
	bp := &baseProvider{name: "test", healthCheck: hc}
	var results []HealthCheckResult
	cancel, interval, _, err := bp.SubscribeHealthCheck(server.URL, status, func(r HealthCheckResult) { results = append(results, r) })
	if err != nil || interval != time.Minute {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := p.URLTest(context.Background(), server.URL, status); err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatal("manual test emitted provider event")
	}
	hc.check()
	if len(results) != 1 {
		t.Fatalf("got %d results", len(results))
	}
	r := results[0]
	if r.Proxy != p || r.URL != server.URL || r.ExpectedStatus != "204" || r.Err != nil || r.Finished.Before(r.Started) {
		t.Fatalf("invalid result: %+v", r)
	}
	cancel()
	cancel()
	hc.emit(r)
	if len(results) != 1 {
		t.Fatal("cancel did not detach")
	}
	if len(hc.listeners) != 0 {
		t.Fatal("listener leaked")
	}
}

func TestHealthSubscriptionValidationAndClose(t *testing.T) {
	status, _ := utils.NewUnsignedRanges[uint16]("204")
	for _, lazy := range []bool{false, true} {
		hc := NewHealthCheck(nil, "https://probe", 500, 60, lazy, status)
		bp := &baseProvider{name: "test", healthCheck: hc}
		cancel, _, _, err := bp.SubscribeHealthCheck("https://probe", nil, func(HealthCheckResult) {})
		if err == nil {
			cancel()
			t.Fatal("status mismatch accepted")
		}
		cancel, _, _, err = bp.SubscribeHealthCheck("https://missing", status, func(HealthCheckResult) {})
		if err == nil {
			cancel()
			t.Fatal("missing task accepted")
		}
		var count atomic.Int32
		cancel, _, _, err = bp.SubscribeHealthCheck("https://probe", status, func(HealthCheckResult) { count.Add(1) })
		if lazy {
			if err == nil {
				cancel()
				t.Fatal("lazy task accepted")
			}
		} else {
			if err != nil {
				t.Fatal(err)
			}
			hc.close()
			hc.emit(HealthCheckResult{URL: "https://probe", ExpectedStatus: "204"})
			cancel()
			if count.Load() != 0 {
				t.Fatal("closed health check emitted")
			}
		}
		hc.close()
	}
}

func TestHealthCancellationConcurrent(t *testing.T) {
	hc := NewHealthCheck(nil, "https://probe", 500, 60, false, nil)
	defer hc.close()
	bp := &baseProvider{name: "test", healthCheck: hc}
	var count atomic.Int32
	cancel, _, _, err := bp.SubscribeHealthCheck("https://probe", nil, func(HealthCheckResult) { count.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				hc.emit(HealthCheckResult{URL: "https://probe", ExpectedStatus: "*"})
			}
		}()
	}
	cancel()
	stopped := count.Load()
	wg.Wait()
	if count.Load() != stopped {
		t.Fatal("callback continued after cancel returned")
	}
}
