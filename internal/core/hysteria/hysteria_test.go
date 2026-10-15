package hysteria

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/core/coretest"
	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestRenderAndAuth(t *testing.T) {
	a := New(core.Options{WorkDir: t.TempDir(), AuthURL: "http://127.0.0.1:10090/auth/hysteria"})
	in := coretest.Inbound(model.CoreHysteria, "hy2", 443, model.ProtocolSpec{Type: model.ProtoHysteria2, Obfs: "salamander", ObfsPassword: "o"},
		model.TransportSpec{}, coretest.TLS(t, "h.example.com"))
	cfg, err := a.Render(&model.NodeSpec{Inbounds: []model.Inbound{in}})
	if err != nil || !strings.Contains(string(cfg), `"type": "http"`) || !strings.Contains(string(cfg), "salamander") {
		t.Fatalf("%v %s", err, cfg)
	}
	auth := func(pw string) (bool, string) {
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"addr":"1.2.3.4:5","auth":"`+pw+`"}`)))
		var r struct {
			OK bool   `json:"ok"`
			ID string `json:"id"`
		}
		json.Unmarshal(rec.Body.Bytes(), &r)
		return r.OK, r.ID
	}
	if ok, id := auth(coretest.Users()[0].Password); !ok || id != "u1" {
		t.Fatal("valid user rejected")
	}
	// Hot removal only touches memory.
	a.RemoveUsers(t.Context(), "hy2", []string{"u1"})
	if ok, _ := auth(coretest.Users()[0].Password); ok {
		t.Fatal("removed user accepted")
	}
	if ok, _ := auth("nope"); ok {
		t.Fatal("bad password accepted")
	}
}

func TestLiveMultiInbound(t *testing.T) {
	bin := os.Getenv("POLARIS_HYSTERIA_BIN")
	if bin == "" {
		t.Skip("POLARIS_HYSTERIA_BIN not set")
	}
	free := func() int {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	a := New(core.Options{BinPath: bin, WorkDir: t.TempDir(), APIListen: fmt.Sprintf("127.0.0.1:%d", free())})
	auth := httptest.NewServer(a)
	defer auth.Close()
	a.opt.AuthURL = auth.URL + "/auth/hysteria"
	mk := func(tag string) model.Inbound {
		in := coretest.Inbound(model.CoreHysteria, tag, free(), model.ProtocolSpec{Type: model.ProtoHysteria2}, model.TransportSpec{}, coretest.TLS(t, "h.example.com"))
		in.Listen = "127.0.0.1"
		return in
	}
	spec := &model.NodeSpec{Inbounds: []model.Inbound{mk("hy-a"), mk("hy-b")}}
	spec.Inbounds[1].Users = spec.Inbounds[1].Users[:1] // only u1 on hy-b
	if _, err := a.Render(spec); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := a.Start(ctx); err != nil {
		t.Fatalf("%v\n%s", err, strings.Join(a.Logs(20), "\n"))
	}
	defer a.Stop(ctx)
	time.Sleep(500 * time.Millisecond)
	if st := a.Health(ctx); !st.Running || st.Inbounds != 2 {
		t.Fatalf("health %+v\n%s", st, strings.Join(a.Logs(20), "\n"))
	}
	// The stats API comes up a moment after the process; poll instead of a fixed sleep.
	var serr error
	for i := 0; i < 50; i++ {
		if _, serr = a.CollectTraffic(ctx); serr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if serr != nil {
		t.Fatalf("stats: %v", serr)
	}
	// Auth is per inbound: u2 is not authorised on hy-b.
	post := func(tag, pw string) bool {
		resp, err := http.Post(auth.URL+"/auth/hysteria?tag="+tag, "application/json", strings.NewReader(`{"auth":"`+pw+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r struct{ OK bool }
		json.NewDecoder(resp.Body).Decode(&r)
		return r.OK
	}
	u2 := coretest.Users()[1].Password
	if !post("hy-a", u2) || post("hy-b", u2) {
		t.Fatal("per-inbound auth broken")
	}
	// Dropping an inbound stops its process.
	spec.Inbounds = spec.Inbounds[:1]
	a.Render(spec)
	a.Reload(ctx)
	time.Sleep(300 * time.Millisecond)
	if st := a.Health(ctx); st.Inbounds != 1 || !st.Running {
		t.Fatalf("after removal: %+v", st)
	}
}
