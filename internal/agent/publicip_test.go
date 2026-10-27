package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestSlowIPDetectionDoesNotBlockHeartbeat(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer echo.Close()
	defer close(release)
	d := newIPDetector([]string{echo.URL}, []string{echo.URL}, time.Minute)
	d.ip4, d.ip6 = "198.51.100.10", "2001:db8::10"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() { defer close(stopped); d.Run(ctx) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("IP discovery did not start")
	}
	// Even an expired cache is read without starting another synchronous lookup.
	d.mu.Lock()
	d.at = time.Now().Add(-2 * d.every)
	d.mu.Unlock()
	a := &Agent{ips: d}
	heartbeat := make(chan error, 1)
	go func() { heartbeat <- a.sendHeartbeat() }()
	select {
	case err := <-heartbeat:
		if err == nil { // no stream is connected in this fixture
			t.Fatal("unexpected connected heartbeat")
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat waited for the slow echo service")
	}
	if ip4, ip6 := d.Cached(); ip4 != "198.51.100.10" || ip6 != "2001:db8::10" {
		t.Fatal("in-flight discovery discarded cached addresses", ip4, ip6)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("IP worker did not stop on agent shutdown")
	}
}

func TestIPDiscoveryRefreshesCachedAddresses(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("198.51.100.20"))
	}))
	defer echo.Close()
	d := newIPDetector([]string{echo.URL}, []string{echo.URL}, time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d.Get(ctx)
	if ip4, _ := d.Cached(); ip4 != "198.51.100.20" {
		t.Fatal("refreshed address not available to heartbeats", ip4)
	}
}

type slowOnlineCore struct {
	core.CoreAdapter
	entered chan struct{}
	calls   atomic.Int32
}

func (c *slowOnlineCore) OnlineIPs(ctx context.Context) (map[string][]string, error) {
	if c.calls.Add(1) == 1 {
		close(c.entered)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *slowOnlineCore) Health(context.Context) model.CoreStatus { return model.CoreStatus{} }

func TestOnlineIPCollectionDoesNotDelayTelemetry(t *testing.T) {
	c := &slowOnlineCore{entered: make(chan struct{})}
	a := &Agent{cfg: Config{OnlineIPs: 10 * time.Millisecond}, installer: &Installer{Dir: t.TempDir()},
		cores: map[model.CoreType]*managed{model.CoreXray: {adapter: c}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan struct{})
	go func() { defer close(stopped); a.onlineIPsLoop(ctx) }()
	select {
	case <-c.entered:
	case <-time.After(time.Second):
		t.Fatal("online IP scan did not start")
	}
	heartbeat := make(chan error, 1)
	go func() { heartbeat <- a.sendHeartbeat() }()
	select {
	case <-heartbeat:
	case <-time.After(time.Second):
		t.Fatal("slow online IP scan blocked resource telemetry")
	}
	// Multiple scan ticks elapse while the first query remains blocked.
	time.Sleep(40 * time.Millisecond)
	if got := c.calls.Load(); got != 1 {
		t.Fatalf("slow online IP scans overlapped: %d", got)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("online IP worker ignored session cancellation")
	}
}
