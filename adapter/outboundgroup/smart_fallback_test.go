package outboundgroup

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/adapter/provider"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
)

type circuitFixture struct {
	s       *smartFallback
	proxies []C.Proxy
	now     time.Time
}

func newCircuitFixture() *circuitFixture {
	x := &circuitFixture{now: time.Unix(10000, 0)}
	for _, name := range []string{"A", "B"} {
		x.proxies = append(x.proxies, adapter.NewProxy(outbound.NewDirectWithOption(outbound.DirectOption{Name: name})))
	}
	x.s = newSmartFallback("test", defaultSmartFallbackOption(), time.Minute, 3*time.Second)
	x.s.now = func() time.Time { return x.now }
	return x
}

func (x *circuitFixture) probe(index int, at time.Time, success bool) {
	var err error
	if !success {
		err = errors.New("controlled failure")
	}
	x.now = at.Add(time.Second)
	x.s.observe(x.proxies, provider.HealthCheckResult{Proxy: x.proxies[index], Started: at, Finished: x.now, Err: err})
}
func (x *circuitFixture) failThree(index int) {
	start := x.now
	for i := 0; i < 3; i++ {
		x.probe(index, start.Add(time.Duration(i)*time.Minute), false)
	}
}
func TestSmartWindowAndPriority(t *testing.T) {
	x := newCircuitFixture()
	start := x.now
	x.probe(0, start, false)
	x.probe(0, start.Add(time.Minute), true)
	x.probe(0, start.Add(2*time.Minute), false)
	x.probe(0, start.Add(3*time.Minute), true)
	x.probe(0, start.Add(4*time.Minute), false)
	n := x.s.nodes[x.proxies[0]]
	if n.phase != circuitCooling || n.level != 1 {
		t.Fatal("three failures interspersed with successes must trip")
	}
	until := n.until
	for i := 0; i < 10; i++ {
		if p := x.s.choose(x.proxies, "", ""); p != x.proxies[1] {
			t.Fatal("must select B")
		}
	}
	if n.until != until {
		t.Fatal("queries extended cooldown")
	}
	x.probe(0, x.now.Add(time.Minute), true)
	if n.phase != circuitCooling {
		t.Fatal("cooldown success restored eligibility")
	}
	for i := 0; i < 3; i++ {
		x.probe(0, until.Add(time.Duration(i)*time.Minute), true)
		if i < 2 && x.s.choose(x.proxies, "", "") != x.proxies[1] {
			t.Fatal("premature recovery")
		}
	}
	if x.s.choose(x.proxies, "", "") != x.proxies[0] {
		t.Fatal("recovered A must regain priority")
	}
}

func TestSmartWindowBoundaryAndDuplicate(t *testing.T) {
	x := newCircuitFixture()
	start := x.now
	x.probe(0, start, false)
	x.probe(0, start.Add(time.Minute), false)
	// The first completion lies exactly on the excluded lower boundary.
	x.probe(0, start.Add(10*time.Minute), false)
	n := x.s.nodes[x.proxies[0]]
	if n.phase != circuitNormal || len(n.failures) != 2 {
		t.Fatal("window boundary wrong")
	}
	r := provider.HealthCheckResult{Proxy: x.proxies[0], Started: start.Add(10 * time.Minute), Finished: x.now, Err: errors.New("duplicate")}
	x.s.observe(x.proxies, r)
	r.Started = r.Started.Add(time.Millisecond)
	x.s.observe(x.proxies, r)
	if len(n.failures) != 2 {
		t.Fatal("duplicate or overlapping probe counted")
	}
	x.probe(0, start.Add(10*time.Minute+2*time.Second), false)
	if n.phase != circuitCooling {
		t.Fatal("third valid failure did not trip")
	}
}

func TestSmartEscalationAndLateProbe(t *testing.T) {
	x := newCircuitFixture()
	x.failThree(0)
	n := x.s.nodes[x.proxies[0]]
	for _, minutes := range []int{60, 120, 120} {
		deadline := n.until
		x.probe(0, deadline.Add(-time.Second), true)
		if n.successes != 0 || n.phase == circuitNormal {
			t.Fatal("probe started before recovery counted")
		}
		x.probe(0, deadline.Add(time.Second), false)
		if n.phase != circuitCooling || n.until.Sub(x.now) != time.Duration(minutes)*time.Minute {
			t.Fatal("wrong cooldown escalation")
		}
	}
}

func TestSmartResetRequiresContinuousEvidence(t *testing.T) {
	x := newCircuitFixture()
	x.failThree(0)
	n := x.s.nodes[x.proxies[0]]
	deadline := n.until
	for i := 0; i < 3; i++ {
		x.probe(0, deadline.Add(time.Duration(i)*time.Minute), true)
	}
	stable := n.stableSince
	x.probe(0, stable.Add(2*time.Hour), true)
	if n.level != 1 {
		t.Fatal("missing probes incorrectly reset level")
	}
	stable = n.stableSince
	for i := 1; i <= 59; i++ {
		x.probe(0, stable.Add(time.Duration(i)*time.Minute), true)
	}
	if n.level != 1 {
		t.Fatal("reset too early")
	}
	x.probe(0, stable.Add(time.Hour), true)
	if n.level != 0 {
		t.Fatal("continuous hour did not reset")
	}
}

func TestSmartEmergencyAndNoBypass(t *testing.T) {
	x := newCircuitFixture()
	x.failThree(0)
	x.failThree(1)
	if x.s.choose(x.proxies, "", "") != nil {
		t.Fatal("all tripped must have no eligible proxy")
	}
	x.now = x.s.nextEmergency
	x.s.choose(x.proxies, "", "")
	a, b := x.s.nodes[x.proxies[0]], x.s.nodes[x.proxies[1]]
	if a.phase != circuitRecovering || b.phase != circuitCooling {
		t.Fatal("must verify only earliest candidate")
	}
	if x.s.checkSelection(x.proxies, "", "A") == nil {
		t.Fatal("manual selection bypassed verification")
	}
	at := x.now.Add(time.Second)
	x.probe(0, at, false)
	if x.s.choose(x.proxies, "", "") != nil || b.phase != circuitCooling {
		t.Fatal("retry storm after emergency failure")
	}
	x.now = x.s.nextEmergency
	x.s.choose(x.proxies, "", "")
	if b.phase != circuitRecovering {
		t.Fatal("must advance to next earliest")
	}
	for i := 0; i < 3; i++ {
		x.probe(1, x.now.Add(time.Minute), true)
	}
	if x.s.choose(x.proxies, "", "") != x.proxies[1] {
		t.Fatal("emergency verification failed to restore B")
	}
}

func TestSmartReplacementAndClose(t *testing.T) {
	x := newCircuitFixture()
	x.failThree(0)
	old := x.proxies[0]
	x.proxies[0] = adapter.NewProxy(outbound.NewDirectWithOption(outbound.DirectOption{Name: "A"}))
	if x.s.choose(x.proxies, "", "") != x.proxies[0] {
		t.Fatal("replacement inherited punishment")
	}
	x.s.observe(x.proxies, provider.HealthCheckResult{Proxy: old, Started: x.now.Add(time.Minute), Finished: x.now.Add(time.Minute + time.Second)})
	if _, ok := x.s.nodes[old]; ok {
		t.Fatal("late event resurrected old node")
	}
	x.s.closed = true
	if x.s.choose(x.proxies, "", "") != nil {
		t.Fatal("closed state used")
	}
}

func TestSmartConcurrentState(t *testing.T) {
	x := newCircuitFixture()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				at := time.Unix(int64(20000+i*100+j), 0)
				x.s.observe(x.proxies, provider.HealthCheckResult{Proxy: x.proxies[i%2], Started: at, Finished: at.Add(time.Millisecond)})
				x.s.choose(x.proxies, "", "")
				_ = x.s.checkSelection(x.proxies, "", "A")
			}
		}(i)
	}
	wg.Wait()
}

func TestSmartConfigurationValidation(t *testing.T) {
	cases := []func(*SmartFallbackOption){
		func(o *SmartFallbackOption) { o.Window = 0 },
		func(o *SmartFallbackOption) { o.Window = math.MaxInt64 },
		func(o *SmartFallbackOption) { o.Cooldown = nil },
		func(o *SmartFallbackOption) { o.Cooldown = []int64{60, 30} },
		func(o *SmartFallbackOption) { o.FailureThreshold = 0 },
		func(o *SmartFallbackOption) { o.RecoverSuccess = -1 },
		func(o *SmartFallbackOption) { o.ResetAfter = 0 },
	}
	for i, change := range cases {
		o := defaultSmartFallbackOption()
		change(&o)
		if o.validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestSmartUnavailableTCPUDP(t *testing.T) {
	p := &unavailableProxy{Base: outbound.NewBase(outbound.BaseOption{Name: "unavailable", Type: C.Reject, UDP: true})}
	if _, err := p.DialContext(context.Background(), &C.Metadata{}); !errors.Is(err, ErrSmartFallbackUnavailable) {
		t.Fatal(err)
	}
	if _, err := p.ListenPacketContext(context.Background(), &C.Metadata{}); !errors.Is(err, ErrSmartFallbackUnavailable) {
		t.Fatal(err)
	}
}

func TestSmartRecoveryIgnoresEarlySamplesWithoutLosingProgress(t *testing.T) {
	x := newCircuitFixture()
	x.failThree(0)
	n := x.s.nodes[x.proxies[0]]
	at := n.until
	x.probe(0, at, true)
	x.probe(0, at.Add(time.Minute-time.Millisecond), true)
	if n.successes != 1 {
		t.Fatal("early probe accelerated recovery")
	}
	x.probe(0, at.Add(2*time.Minute), true)
	if n.successes != 2 {
		t.Fatal("scheduler jitter discarded valid progress")
	}
	x.probe(0, at.Add(3*time.Minute), true)
	if n.phase != circuitNormal {
		t.Fatal("three spaced successes must restore eligibility")
	}
}

type fixedHealthProxy struct {
	C.Proxy
	healthy bool
}

func (p *fixedHealthProxy) AliveForTestUrl(string) bool { return p.healthy }

func TestDisabledFallbackRetainsUpstreamOrderingAndFailOpen(t *testing.T) {
	a := &fixedHealthProxy{Proxy: adapter.NewProxy(outbound.NewDirectWithOption(outbound.DirectOption{Name: "A"}))}
	b := &fixedHealthProxy{Proxy: adapter.NewProxy(outbound.NewDirectWithOption(outbound.DirectOption{Name: "B"}))}
	hc := provider.NewHealthCheck([]C.Proxy{a, b}, "https://probe", 3000, 60, false, nil)
	pd, err := provider.NewCompatibleProvider("legacy", []C.Proxy{a, b}, hc)
	if err != nil {
		t.Fatal(err)
	}
	defer pd.Close()
	f := &Fallback{GroupBase: NewGroupBase(GroupBaseOption{Name: "legacy", Type: C.Fallback, Providers: []P.ProxyProvider{pd}}), testUrl: "https://probe"}
	if f.Now() != "A" {
		t.Fatal("disabled all-failed fallback must retain first-node behavior")
	}
	b.healthy = true
	if f.Now() != "B" {
		t.Fatal("disabled selection must skip unhealthy A")
	}
	a.healthy = true
	if f.Now() != "A" {
		t.Fatal("disabled selection must return to A")
	}
}

func TestSmartGroupStateIsolation(t *testing.T) {
	x := newCircuitFixture()
	other := newSmartFallback("other", defaultSmartFallbackOption(), time.Minute, 3*time.Second)
	other.now = x.s.now
	x.failThree(0)
	if x.s.choose(x.proxies, "", "") != x.proxies[1] {
		t.Fatal("first group did not isolate A")
	}
	if other.choose(x.proxies, "", "") != x.proxies[0] {
		t.Fatal("punishment leaked across groups")
	}
}
