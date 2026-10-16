package panel

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestSubscriptionSettings(t *testing.T) {
	s := newTestServer(t)
	srv := httptest.NewServer(s.Router())
	defer srv.Close()
	s.cfg.PublicURL = srv.URL

	exp := time.Now().Add(10 * 24 * time.Hour)
	u := store.User{ID: "u1", Username: "alice", UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", SubToken: "alice-subscription-token",
		Status: "active", TrafficLimit: 100 << 30, UsedDown: 30 << 30, ExpireAt: &exp, ResetPolicy: "monthly", ResetDay: 1}
	s.st.DB.Create(&u)
	s.st.DB.Create(&store.TrafficHourly{UserID: "u1", NodeID: "n1", Hour: time.Now().UTC().Truncate(time.Hour), Up: 1 << 20, Down: 5 << 20})
	manual := store.SubSource{ID: "s1", Name: "mine", Kind: "manual", Enabled: true, Content: "trojan://pw@hk1.example.com:443#HK 01"}
	s.st.DB.Create(&manual)
	s.st.DB.Create(&store.SubProfile{ID: "p1", Name: "mix", Token: "profile-token-abcdefgh", Enabled: true, PanelUserID: "u1",
		SourceIDs: store.NewJSON([]string{"s1"}), Templates: store.NewJSON(map[string]string{})})

	get := func(path, ua string) (int, string, http.Header) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("User-Agent", ua)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), resp.Header
	}

	s.st.SetSetting("sub_base_url", "https://sub.example.com/")
	s.st.SetSetting("sub_title", "北极星")
	s.st.SetSetting("sub_announce", "本周六维护\n请提前更新订阅")
	s.st.SetSetting("sub_support_url", "https://t.me/support")
	s.st.SetSetting("sub_routing_enabled", "true")
	s.st.SetSetting("sub_routing_rules", `{"Name":"cn"}`)
	s.st.SetSetting("sub_show_info", "nodes")

	if got := s.profileURL("x"); got != "https://sub.example.com/sub/p/x" {
		t.Fatalf("sub base url: %s", got)
	}
	code, body, hdr := get("/sub/p/profile-token-abcdefgh", "clash-verge/2.0")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if hdr.Get("profile-title") != "base64:"+b64("北极星") || hdr.Get("support-url") != "https://t.me/support" ||
		hdr.Get("announce") != "base64:"+b64("本周六维护\n请提前更新订阅") ||
		hdr.Get("routing") != "happ://routing/onadd/"+base64.StdEncoding.EncodeToString([]byte(`{"Name":"cn"}`)) {
		t.Fatalf("headers: %v", hdr)
	}
	for _, want := range []string{"📦 套餐 100 GB · 已用 30 GB（30%）", "📊 剩余流量 70 GB", " 到期 · 剩 ", "📢 本周六维护 请提前更新订阅", "HK 01"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}

	s.st.SetSetting("sub_show_info", "suffix")
	_, body, _ = get("/sub/p/profile-token-abcdefgh", "clash-verge/2.0")
	if !strings.Contains(body, "HK 01 | 剩 70 GB · ") || strings.Contains(body, "📊") {
		t.Fatalf("suffix mode:\n%s", body)
	}

	_, body, _ = get("/sub/p/profile-token-abcdefgh", "Shadowrocket/2070 CFNetwork/1568 Darwin/24.0.0")
	if dec, err := base64.StdEncoding.DecodeString(body); err != nil || !strings.HasPrefix(string(dec), "STATUS=已用 30 GB / 100 GB（30%） · 剩余 70 GB · ") {
		t.Fatalf("shadowrocket status line: %v %.200q", err, dec)
	}

	// Browser info page: announcement, status, chart, per-node links and import buttons.
	_, page, _ := get("/sub/alice-subscription-token", "Mozilla/5.0 Chrome/120")
	for _, want := range []string{"本周六维护", "正常", "近 30 天流量", "每月 1 日重置流量", "clash://install-config", "happ://add/", "https://t.me/support"} {
		if !strings.Contains(page, want) {
			t.Fatalf("page missing %q", want)
		}
	}
	_, page, _ = get("/sub/p/profile-token-abcdefgh", "Mozilla/5.0 Chrome/120")
	if !strings.Contains(page, "HK 01") || !strings.Contains(page, "trojan://") {
		t.Fatal("profile page should list node links")
	}

	s.st.SetSetting("sub_enabled", "false")
	if code, _, _ := get("/sub/alice-subscription-token", "clash"); code != 404 {
		t.Fatalf("disabled subscriptions should 404, got %d", code)
	}
}

func TestExpireAfterFirstUse(t *testing.T) {
	s := newTestServer(t)
	s.st.DB.Create(&store.Node{ID: "n1", Name: "n1", TrafficRate: 1})
	s.st.DB.Create(&store.User{ID: "u1", Username: "bob", UUID: "u-1", SubToken: "bob-token-0123456789", Status: "active", ExpireAfterDays: 30})
	s.traffic.Add("n1", []model.TrafficDelta{{UserID: "u1", Up: 10, Down: 10}})
	if err := s.settleTraffic(); err != nil {
		t.Fatal(err)
	}
	var u store.User
	s.st.DB.First(&u, "id = ?", "u1")
	if u.ExpireAt == nil || u.ExpireAfterDays != 0 || time.Until(*u.ExpireAt) < 29*24*time.Hour {
		t.Fatalf("expiry not started: %+v", u)
	}
}
