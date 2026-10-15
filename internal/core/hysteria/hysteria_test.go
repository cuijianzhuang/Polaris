package hysteria

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

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
