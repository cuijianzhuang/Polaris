package singbox

import (
	"encoding/json"
	"github.com/cuijianzhuang/polaris/internal/core"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSnellDedicatedAppliedOwnership(t *testing.T) {
	a := New(core.Options{Version: "1.14.4"})
	s := snellSpec(t)
	s.Inbounds[0].Protocol.SnellAuth = "psk"
	s.Inbounds[0].Protocol.SnellUserID = "u1"
	s.Inbounds[0].Users = s.Inbounds[0].Users[:1]
	a.snellOwners = map[string]string{"snell": "previous-user"}
	cfg, e := a.Render(s)
	if e != nil {
		t.Fatal(e)
	}
	if a.snellOwners["snell"] != "previous-user" || a.pendingSnellOwners["snell"] != "u1" {
		t.Fatal("render changed running ownership before old traffic drain")
	}
	var doc map[string]any
	json.Unmarshal(cfg, &doc)
	in := doc["inbounds"].([]any)[0].(map[string]any)
	if in["users"] != nil || in["psk"] == s.Inbounds[0].Protocol.ServerKey {
		t.Fatal("dedicated mode reused shared encryption seed or userkey")
	}
	counts := snellOwnerTraffic(map[string][2]int64{"inbound>>>snell": {10, 20}, "u1": {3, 4}}, a.pendingSnellOwners)
	if counts["u1"] != [2]int64{13, 24} || counts["inbound>>>snell"] != [2]int64{10, 20} {
		t.Fatal("ownership accounting failed")
	}
	s.Inbounds[0].Protocol.SnellMode = "unsafe-raw"
	s.Inbounds[0].Protocol.SnellVersion = 6
	if _, e := a.Render(s); e == nil {
		t.Fatal("dedicated PSK accepted unencrypted raw mode")
	}
	s.Inbounds[0].Protocol.SnellMode = ""
	s.Inbounds[0].Users = nil
	cfg, e = a.Render(s)
	if e != nil || strings.Contains(string(cfg), `"type": "snell"`) {
		t.Fatal("ungranted owner was served", e)
	}
}

func TestSnellDedicatedAuthenticatedOnline(t *testing.T) {
	tr := newOnlineTracker()
	tr.singleOwners = map[string]string{"alice-port": "u1"}
	tr.line("INFO [1 0ms] inbound/snell[alice-port]: inbound connection from 192.0.2.1:3333")
	if len(tr.online(time.Now())) != 0 {
		t.Fatal("unauthenticated TCP open counted as user")
	}
	tr.line("INFO [1 1ms] inbound/snell[alice-port]: inbound connection to example.com:443")
	if got := tr.online(time.Now()); len(got["u1"]) != 1 || got["u1"][0] != "192.0.2.1" {
		t.Fatal(got)
	}
}

func TestSnellDedicatedUDPAuthenticationSource(t *testing.T) {
	tr := newOnlineTracker()
	tr.singleOwners = map[string]string{"alice-port": "u1"}
	tr.line("INFO [9 0ms] inbound/snell[alice-port]: inbound connection from 192.0.2.9:3333")
	if len(tr.online(time.Now())) != 0 {
		t.Fatal("unauthenticated TCP open counted")
	}
	tr.line("INFO [9 1ms] inbound/snell[alice-port]: inbound packet connection from 192.0.2.9:3333")
	if got := tr.online(time.Now()); len(got["u1"]) != 1 || got["u1"][0] != "192.0.2.9" {
		t.Fatal(got)
	}
}

func TestSnellDedicatedMeteringTags(t *testing.T) {
	for _, tag := range []string{"", "api", "a>>>b", "bad]tag"} {
		s := snellSpec(t)
		s.Inbounds[0].Tag = tag
		s.Inbounds[0].Protocol.SnellAuth = "psk"
		s.Inbounds[0].Protocol.SnellUserID = "u1"
		s.Inbounds[0].Users = s.Inbounds[0].Users[:1]
		if _, err := New(core.Options{Version: "1.14.4"}).Render(s); err == nil {
			t.Fatal("unmetered/unattributable dedicated tag accepted", tag)
		}
	}
	tr := newOnlineTracker()
	tr.singleOwners = map[string]string{"Hong Kong: Alice": "u1"}
	tr.line("INFO [99 0ms] inbound/snell[Hong Kong: Alice]: inbound connection from 192.0.2.99:3333")
	if len(tr.online(time.Now())) != 0 {
		t.Fatal("unauthenticated open counted")
	}
	tr.line("INFO [99 1ms] inbound/snell[Hong Kong: Alice]: inbound connection to example.com:443")
	if got := tr.online(time.Now()); len(got["u1"]) != 1 || got["u1"][0] != "192.0.2.99" {
		t.Fatal(got)
	}
}

func TestSnellRejectedConfigCannotChangeRestartOwnership(t *testing.T) {
	a := New(core.Options{Version: "1.14.4", WorkDir: t.TempDir()})
	s := snellSpec(t)
	all := s.Inbounds[0].Users
	s.Inbounds[0].Protocol.SnellAuth = "psk"
	s.Inbounds[0].Protocol.SnellUserID = "u1"
	s.Inbounds[0].Users = all[:1]
	installed, e := a.Render(s)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(a.configPath(), installed, 0600); e != nil {
		t.Fatal(e)
	}
	if e = a.activateSnellOwners(); e != nil {
		t.Fatal(e)
	}
	s.Inbounds[0].Protocol.SnellUserID = "u2"
	s.Inbounds[0].Users = all[1:]
	if _, e = a.Render(s); e != nil {
		t.Fatal(e)
	}
	// Simulate rejection during Validate: the installed file remains Alice's.
	if e = a.activateSnellOwners(); e != nil || a.snellOwners["snell"] != "u1" {
		t.Fatal("manual restart stole old ownership", a.snellOwners, e)
	}
	// A normal userkey config needs no PSK ownership map, even if a rejected
	// PSK draft remains pending after prior SIGHUP-only routing changes.
	os.WriteFile(a.configPath(), []byte(`{"inbounds":[{"type":"snell","users":[{"name":"u1","userkey":"key"}]}]}`), 0600)
	if e = a.activateSnellOwners(); e != nil || len(a.snellOwners) != 0 {
		t.Fatal("ordinary userkey restart rejected", e)
	}
	// Arbitrary file replacement must not silently serve an unmetered PSK port.
	os.WriteFile(a.configPath(), []byte(`{"inbounds":[{"type":"snell","psk":"unknown"}]}`), 0600)
	if e = a.activateSnellOwners(); e == nil {
		t.Fatal("unknown single-PSK config accepted")
	}
}
