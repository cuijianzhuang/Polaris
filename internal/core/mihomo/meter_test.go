package mihomo

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
)

func TestMeterKeepsShortObservedConnectionUntilDrain(t *testing.T) {
	var mu sync.Mutex
	active, fail := true, false
	observed := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			http.Error(w, "unavailable", 503)
			return
		}
		conns := []connection{}
		if active {
			c := connection{ID: "short", Upload: 120, Download: 420}
			c.Metadata.InboundUser = "alice"
			conns = append(conns, c)
		}
		json.NewEncoder(w).Encode(map[string]any{"connections": conns})
		select {
		case observed <- struct{}{}:
		default:
		}
	}))
	defer server.Close()
	a := New(core.Options{APIListen: strings.TrimPrefix(server.URL, "http://")})
	a.startMeter()
	defer a.stopMeter()
	select {
	case <-observed:
	case <-time.After(4 * time.Second):
		t.Fatal("background sampler did not run")
	}
	mu.Lock()
	active = false
	mu.Unlock()
	out, err := a.CollectTraffic(t.Context())
	if err != nil || len(out) != 1 || out[0].Up != 120 || out[0].Down != 420 {
		t.Fatalf("observed closed connection lost: %+v %v", out, err)
	}
	out, err = a.CollectTraffic(t.Context())
	if err != nil || len(out) != 0 {
		t.Fatal("drain duplicated traffic", out, err)
	}
	if st := a.TrafficMeterStatus(); st.LastSuccess == nil || st.Mode != "connection_sampled" || st.IntervalSeconds != 1 {
		t.Fatal(st)
	}
	mu.Lock()
	active = true
	mu.Unlock()
	if err := a.sampleTraffic(t.Context()); err != nil {
		t.Fatal(err)
	}
	success := a.TrafficMeterStatus().LastSuccess
	mu.Lock()
	fail = true
	mu.Unlock()
	out, err = a.CollectTraffic(t.Context())
	if err != nil || len(out) != 1 || out[0].Up != 120 {
		t.Fatal("API failure discarded buffered observations", out, err)
	}
	if st := a.TrafficMeterStatus(); st.ConsecutiveFailures == 0 || !st.LastSuccess.Equal(*success) {
		t.Fatal("failure claimed a successful collection", st)
	}
	if _, err = a.CollectTraffic(t.Context()); err == nil {
		t.Fatal("empty failed read reported success")
	}
}

func TestMeterRejectsNegativeResetAndDuplicateCounters(t *testing.T) {
	snapshots := []string{
		`[{"id":"c","upload":100,"download":200,"metadata":{"inboundUser":"alice"}},{"id":"c","upload":100,"metadata":{"inboundUser":"alice"}},{"id":"bad","upload":-5,"metadata":{"inboundUser":"bob"}}]`,
		`[{"id":"c","upload":80,"download":90,"metadata":{"inboundUser":"alice"}}]`,
		`[{"id":"c","upload":90,"download":105,"metadata":{"inboundUser":"alice"}}]`,
	}
	i := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"connections":` + snapshots[i] + `}`)) }))
	defer server.Close()
	a := New(core.Options{APIListen: strings.TrimPrefix(server.URL, "http://")})
	for _, want := range [][2]int64{{100, 200}, {0, 0}, {10, 15}} {
		out, err := a.CollectTraffic(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if want[0] == 0 {
			if len(out) != 0 {
				t.Fatal("reset billed an invented delta", out)
			}
		} else if len(out) != 1 || out[0].UserID != "alice" || out[0].Up != want[0] || out[0].Down != want[1] {
			t.Fatal(want, out)
		}
		i++
	}
}

func TestMeterStopCancelsPendingSample(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer server.Close()
	a := New(core.Options{APIListen: strings.TrimPrefix(server.URL, "http://")})
	a.startMeter()
	select {
	case <-entered:
	case <-time.After(4 * time.Second):
		t.Fatal("sample not started")
	}
	done := make(chan struct{})
	go func() { a.stopMeter(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("sampler leaked during shutdown")
	}
	// Repeated stop is idempotent.
	a.stopMeter()
}
