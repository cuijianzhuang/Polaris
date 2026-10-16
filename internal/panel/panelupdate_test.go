package panel

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func withVersion(t *testing.T, v string) {
	old := Version
	Version = v
	t.Cleanup(func() { Version = old })
}

func withUpdater(t *testing.T, installed bool) {
	old := updaterUnit
	updaterUnit = filepath.Join(t.TempDir(), "polaris-update.path")
	if installed {
		os.WriteFile(updaterUnit, []byte("[Path]\n"), 0o644)
	}
	t.Cleanup(func() { updaterUnit = old })
}

func events(s *Server) chan Event {
	ch := make(chan Event, 16)
	s.sinks = append(s.sinks, ch)
	return ch
}

func next(t *testing.T, ch chan Event) string {
	t.Helper()
	select {
	case e := <-ch:
		return e.Text
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
	return ""
}

func TestParseUpdateResult(t *testing.T) {
	r := parseUpdateResult([]byte("ok=1\nfrom=v0.1.15\nto=v0.1.16\nlog:\n  ✓ 已升级\n\n  done\n"))
	if !r.OK || r.From != "v0.1.15" || r.To != "v0.1.16" || r.Log != "✓ 已升级\ndone" {
		t.Fatalf("%+v", r)
	}
	if r := parseUpdateResult([]byte("ok=0\nfrom=v1\nto=v1\n")); r.OK {
		t.Fatal("failed update read as ok")
	}
}

func TestRequestPanelUpdate(t *testing.T) {
	s := newTestServer(t)
	withVersion(t, "v0.1.15")

	withUpdater(t, false)
	if err := s.requestPanelUpdate("", "me", false); err != errUpdaterMissing {
		t.Fatalf("without the updater: %v", err)
	}
	withUpdater(t, true)
	if err := s.requestPanelUpdate("", "me", true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(s.updateRequestPath()); string(b) != "latest\n" {
		t.Fatalf("request file: %q", b)
	}
	if s.st.GetSetting("update_then_agents", "") != "true" {
		t.Fatal("agents flag not stored")
	}
	if err := s.requestPanelUpdate("v0.1.16", "me", false); err == nil || !strings.Contains(err.Error(), "进行中") {
		t.Fatalf("second request while one is pending: %v", err)
	}
	withVersion(t, "dev")
	os.Remove(s.updateRequestPath())
	if err := s.requestPanelUpdate("", "me", false); err == nil {
		t.Fatal("dev builds must not self-update")
	}
}

func TestCheckUpdateResult(t *testing.T) {
	s := newTestServer(t)
	ch := events(s)
	os.WriteFile(s.updateResultPath(), []byte("ok=1\nfrom=v0.1.15\nto=v0.1.16\nlog:\nok\n"), 0o644)
	s.checkUpdateResult()
	if txt := next(t, ch); !strings.Contains(txt, "v0.1.15 → v0.1.16") {
		t.Fatalf("success: %q", txt)
	}
	if _, err := os.Stat(s.updateResultPath()); err == nil {
		t.Fatal("result file kept: it would be reported again")
	}
	os.WriteFile(s.updateResultPath(), []byte("ok=0\nfrom=v0.1.15\nto=v0.1.16\nlog:\n✗ 下载失败\n"), 0o644)
	s.checkUpdateResult()
	if txt := next(t, ch); !strings.Contains(txt, "失败") || !strings.Contains(txt, "下载失败") {
		t.Fatalf("failure: %q", txt)
	}
	if s.st.GetSetting("auto_update_failed", "") != "0.1.16" {
		t.Fatal("a failed release must not be retried automatically")
	}
}

func TestAutoUpdateTick(t *testing.T) {
	s := newTestServer(t)
	ch := events(s)
	withVersion(t, "v0.1.15")
	withUpdater(t, true)
	latestPanel.Lock()
	latestPanel.v, latestPanel.at = "v0.1.16", time.Now()
	latestPanel.Unlock()
	t.Cleanup(func() { latestPanel.Lock(); latestPanel.v = ""; latestPanel.Unlock() })

	s.autoUpdateTick() // default mode: notify once
	if txt := next(t, ch); !strings.Contains(txt, "v0.1.16") {
		t.Fatalf("announcement: %q", txt)
	}
	s.autoUpdateTick()
	select {
	case e := <-ch:
		t.Fatalf("announced twice: %q", e.Text)
	default:
	}
	if _, err := os.Stat(s.updateRequestPath()); err == nil {
		t.Fatal("notify mode must not update")
	}

	s.st.SetSetting("auto_update", "all")
	s.st.SetSetting("auto_update_hour", "99") // not now
	s.autoUpdateTick()
	if _, err := os.Stat(s.updateRequestPath()); err == nil {
		t.Fatal("updated outside the update hour")
	}
	s.st.SetSetting("auto_update_hour", strconv.Itoa(time.Now().Hour()))
	s.autoUpdateTick()
	if b, _ := os.ReadFile(s.updateRequestPath()); string(b) != "v0.1.16\n" || s.st.GetSetting("update_then_agents", "") != "true" {
		t.Fatalf("auto update not requested: %q", b)
	}
	os.Remove(s.updateRequestPath())
	s.st.SetSetting("auto_update_failed", "0.1.16")
	s.autoUpdateTick()
	if _, err := os.Stat(s.updateRequestPath()); err == nil {
		t.Fatal("retried a release that failed")
	}
}

func TestRequestAccessChange(t *testing.T) {
	s := newTestServer(t)
	withUpdater(t, true)
	for _, c := range []struct{ action, value, line string }{
		{"set-path", "", "set-path /\n"},
		{"set-path", "my-admin", "set-path /my-admin\n"},
		{"set-domain", "Panel.Example.com.", "set-domain panel.example.com\n"},
		{"set-port", "8443", "set-port 8443\n"},
		{"set-url", "http://203.0.113.10:8080/", "set-url http://203.0.113.10:8080\n"},
	} {
		if err := s.requestAccessChange(c.action, c.value, "me"); err != nil {
			t.Fatalf("%s %q: %v", c.action, c.value, err)
		}
		if b, _ := os.ReadFile(s.updateRequestPath()); string(b) != c.line {
			t.Fatalf("%s %q wrote %q", c.action, c.value, b)
		}
		os.Remove(s.updateRequestPath())
	}
	for _, c := range [][2]string{{"set-path", "a b"}, {"set-path", "/x;rm"}, {"set-path", "abc"}, {"set-domain", "bad_domain"},
		{"set-port", "70000"}, {"set-url", "ftp://x"}, {"set-url", "http://x/$(id)"}, {"rm", "-rf"}} {
		if err := s.requestAccessChange(c[0], c[1], "me"); err == nil {
			t.Fatalf("accepted %s %q", c[0], c[1])
		}
	}
	if _, err := os.Stat(s.updateRequestPath()); err == nil {
		t.Fatal("a rejected change wrote a request")
	}
	withUpdater(t, false)
	if err := s.requestAccessChange("set-port", "8443", "me"); err != errUpdaterMissing {
		t.Fatalf("without the updater: %v", err)
	}
}

func TestAccessChangeResult(t *testing.T) {
	s := newTestServer(t)
	ch := events(s)
	os.WriteFile(s.updateResultPath(), []byte("ok=1\naction=set-path\narg=/\nfrom=v1\nto=v1\nlog:\nok\n"), 0o644)
	s.checkUpdateResult()
	if txt := next(t, ch); !strings.Contains(txt, "后台路径已改为 /") {
		t.Fatalf("%q", txt)
	}
	if v := s.st.GetSetting("access_last", ""); !strings.Contains(v, `"action":"set-path"`) || !strings.Contains(v, `"ok":true`) {
		t.Fatalf("last change not kept for the settings page: %s", v)
	}
}
