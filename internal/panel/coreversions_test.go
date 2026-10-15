package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestCoreUpgradeRollbackPanel(t *testing.T) {
	s, _, c := newAdminClient(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "tokyo"})
	db.Create(&store.Node{ID: "n2", Name: "hk"})
	for _, n := range []string{"n1", "n2"} {
		db.Create(&store.Core{ID: "c-" + n, NodeID: n, Type: model.CoreXray, Version: "26.3.27", RunningVersion: "26.3.27",
			InstalledVersions: store.NewJSON([]string{"25.10.15", "26.3.27"})})
	}
	get := func(id string) store.Core {
		var cur store.Core
		db.First(&cur, "id = ?", id)
		return cur
	}

	// Batch upgrade records the previous version.
	code, out := c.do("POST", "/cores/upgrade", map[string]any{"type": "xray", "version": "v26.5.1"})
	if code != 200 || len(out["results"].([]any)) != 2 {
		t.Fatalf("batch upgrade %d %v", code, out)
	}
	if cur := get("c-n1"); cur.Version != "26.5.1" || cur.PrevVersion != "26.3.27" {
		t.Fatalf("after upgrade: %+v", cur)
	}

	// An unrelated failure keeps the choice; a failed switch reverts it.
	s.revertFailedUpgrades("n1", "端口 443/tcp 已被其他程序占用")
	if get("c-n1").Version != "26.5.1" {
		t.Fatal("unrelated failure reverted the version")
	}
	s.revertFailedUpgrades("n1", "xray: 升级到 26.5.1 失败，已回退到 26.3.27：core not running")
	if cur := get("c-n1"); cur.Version != "26.3.27" || cur.PrevVersion != "26.5.1" || cur.Error == "" {
		t.Fatalf("after failed switch: %+v", cur)
	}

	// One-click rollback goes to the previous version...
	if code, out := c.do("POST", "/cores/c-n2/rollback", nil); code != 200 || out["version"] != "26.3.27" {
		t.Fatalf("rollback %d %v", code, out)
	}
	// ...and without one, to the newest older installed version.
	db.Model(&store.Core{}).Where("id = ?", "c-n2").Update("prev_version", "")
	if code, out := c.do("POST", "/cores/c-n2/rollback", nil); code != 200 || out["version"] != "25.10.15" {
		t.Fatalf("rollback to installed %d %v", code, out)
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range [][2]string{{"1.9.0", "1.10.0"}, {"26.3.27", "26.5.1"}, {"v2.6.1", "2.7"}, {"1.13.5", "1.13.10"}} {
		if !versionLess(c[0], c[1]) || versionLess(c[1], c[0]) {
			t.Errorf("%s < %s", c[0], c[1])
		}
	}
}

func TestLatestRelease(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/apernet/hysteria/releases":
			w.Write([]byte(`[{"tag_name":"app/v2.7.0-beta"},{"tag_name":"app/v2.6.4"},{"tag_name":"core/v2.9.9"},{"tag_name":"app/v2.6.10"}]`))
		case "/repos/XTLS/Xray-core/releases":
			w.Write([]byte(`[{"tag_name":"v26.6.1","prerelease":true},{"tag_name":"v26.5.3"}]`))
		default:
			w.WriteHeader(http.StatusForbidden) // rate limited
		}
	}))
	defer api.Close()
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/SagerNet/sing-box/releases/latest" {
			http.Redirect(w, r, "/SagerNet/sing-box/releases/tag/v1.14.2", http.StatusFound)
			return
		}
		http.NotFound(w, r)
	}))
	defer web.Close()
	githubAPI, githubWeb = api.URL, web.URL
	defer func() { githubAPI, githubWeb = "https://api.github.com", "https://github.com" }()
	s := &Server{}
	for repo, want := range map[string]string{"apernet/hysteria": "2.6.10", "XTLS/Xray-core": "26.6.1", "SagerNet/sing-box": "1.14.2"} {
		prefix := "v"
		if repo == "apernet/hysteria" {
			prefix = "app/v"
		}
		if got, err := s.latestRelease(t.Context(), repo, prefix); err != nil || got != want {
			t.Errorf("%s: %q %v (want %s)", repo, got, err, want)
		}
	}
}

// Xray flags every release as a GitHub pre-release: its plain tags count.
func TestXrayPrereleaseFlagIgnored(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"tag_name":"v26.10.10","prerelease":true},{"tag_name":"v26.9.30","prerelease":true},{"tag_name":"v27.0.0-beta","prerelease":true}]`))
	}))
	defer api.Close()
	web := httptest.NewServer(http.NotFoundHandler()) // no feed / redirect fallback
	defer web.Close()
	githubAPI, githubWeb = api.URL, web.URL
	defer func() { githubAPI, githubWeb = "https://api.github.com", "https://github.com" }()
	vs, err := (&Server{}).releaseVersions(t.Context(), "XTLS/Xray-core", "v")
	if err != nil || len(vs) != 2 || vs[0] != "26.10.10" {
		t.Fatalf("versions = %v %v", vs, err)
	}
	if vs, _ := (&Server{}).releaseVersions(t.Context(), "SagerNet/sing-box", "v"); len(vs) != 0 {
		t.Fatalf("other repos must still skip pre-releases: %v", vs)
	}
}

func TestVersionsFromAtom(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<feed><entry><link href="https://github.com/XTLS/Xray-core/releases/tag/v26.9.9"/></entry>` +
			`<entry><link href="https://github.com/XTLS/Xray-core/releases/tag/v26.10.10"/></entry></feed>`))
	}))
	defer web.Close()
	vs, err := versionsFromAtom(t.Context(), web.URL, "XTLS/Xray-core", "v")
	if err != nil || len(vs) != 2 || vs[0] != "26.10.10" {
		t.Fatalf("versions = %v %v", vs, err)
	}
}
