package sub

import (
	"encoding/json"
	"github.com/cuijianzhuang/polaris/internal/model"
	"strings"
	"testing"
)

func TestSnellClientCompatibilityAndRoundtrip(t *testing.T) {
	n := Node{Name: "Snell", Protocol: model.ProtoSnell, Server: "127.0.0.1", Port: 12345, SnellPSK: "server-key", SnellUserKey: "private-user-key", SnellVersion: 4, SnellObfs: "http"}
	renderer, _ := Get("singbox")
	b, err := renderer.Render([]Node{n}, Info{}, "")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseSubscription(b)
	if err != nil || len(parsed) != 1 {
		t.Fatal(err, len(parsed))
	}
	o := SingboxOutbound(parsed[0])
	if o["userkey"] != n.SnellUserKey || o["psk"] != n.SnellPSK || o["version"] != 4 || o["obfs_mode"] != "http" {
		t.Fatal("lost user credential/version")
	}
	for _, format := range []string{"links", "clash", "surge", "loon", "quanx"} {
		r, _ := Get(format)
		b, err := r.Render([]Node{n}, Info{}, "")
		if err != nil {
			t.Fatal(format, err)
		}
		if strings.Contains(string(b), n.SnellPSK) || strings.Contains(string(b), n.SnellUserKey) {
			t.Fatal("unverified client format received Snell credentials", format)
		}
	}
	for _, ua := range []string{"sing-box/1.13.5", "singbox/1.12.0", "sing-box 1.9.1", "SFA/1.13.5", "SFM/1.11.0", "Hiddify/2.0", "sing-box/dev", "sing-box/1.14.0-alpha.1"} {
		if len(FilterSnellClient([]Node{n}, ua)) != 0 {
			t.Fatal("old client received Snell", ua)
		}
	}
	for _, ua := range []string{"", "Mozilla/5.0", "sing-box/1.14.0", "sing-box/1.14.4"} {
		if len(FilterSnellClient([]Node{n}, ua)) != 1 {
			t.Fatal("valid client filtered", ua)
		}
	}
	other := n
	other.SnellUserKey = "second-user"
	if len(Deduplicate([]Node{n, n, other})) != 2 {
		t.Fatal("different user keys deduplicated")
	}
	// A source entry without a userkey must never be reissued as a shared PSK.
	o["userkey"] = ""
	raw, _ := json.Marshal(map[string]any{"outbounds": []any{o}})
	parsed, err = ParseSubscription(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 0 {
		t.Fatal("shared-key source imported")
	}
}
