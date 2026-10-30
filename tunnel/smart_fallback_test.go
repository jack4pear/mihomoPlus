package tunnel_test

import (
	"context"
	"errors"
	"github.com/metacubex/mihomo/tunnel"
	"testing"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/adapter/outboundgroup"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
)

func TestReloadDetachesSmartFallback(t *testing.T) {
	oldProxies, oldProviders := tunnel.Proxies(), tunnel.Providers()
	t.Cleanup(func() { tunnel.UpdateProxies(oldProxies, oldProviders) })
	direct := adapter.NewProxy(outbound.NewDirect())
	empty := adapter.NewProxy(outbound.NewReject())
	newProviders := map[string]P.ProxyProvider{}
	group, err := outboundgroup.ParseProxyGroup(map[string]any{
		"name": "smart", "type": "fallback", "proxies": []string{"DIRECT"},
		"url": "https://probe", "interval": 60, "timeout": 3000, "lazy": false,
		"smart-fallback": map[string]any{"enabled": true},
	}, map[string]C.Proxy{"DIRECT": direct, "COMPATIBLE": empty}, newProviders, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer group.Close()
	for _, pd := range newProviders {
		defer pd.(interface{ Close() error }).Close()
	}
	wrapped := adapter.NewProxy(group)
	tunnel.UpdateProxies(map[string]C.Proxy{"smart": wrapped}, newProviders)
	tunnel.UpdateProxies(map[string]C.Proxy{"smart": wrapped}, newProviders)
	if group.Now() != "DIRECT" {
		t.Fatal("reused group was closed")
	}
	tunnel.UpdateProxies(map[string]C.Proxy{}, map[string]P.ProxyProvider{})
	if _, err := group.DialContext(context.Background(), &C.Metadata{}); !errors.Is(err, outboundgroup.ErrSmartFallbackUnavailable) {
		t.Fatal("replaced group still active", err)
	}
}
