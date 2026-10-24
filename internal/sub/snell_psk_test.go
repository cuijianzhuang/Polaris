package sub

import (
	"github.com/cuijianzhuang/polaris/internal/model"
	"strings"
	"testing"
)

func TestSnellDedicatedSurgePolicies(t *testing.T) {
	n := Node{Name: "Alice", Protocol: model.ProtoSnell, Server: "example.com", Port: 23456, SnellSinglePSK: true, SnellPSK: "dedicated-key", SnellVersion: 4, SnellObfs: "http"}
	if got := SurgeLine(n); got != "Alice = snell, example.com, 23456, psk=dedicated-key, version=4, obfs=http" {
		t.Fatal(got)
	}
	if o := SingboxOutbound(n); o["userkey"] != nil || o["psk"] != n.SnellPSK {
		t.Fatal("standard PSK incorrectly requires userkey")
	}
	n.SnellVersion = 6
	n.SnellObfs = ""
	n.SnellMode = "unshaped"
	if !strings.Contains(SurgeLine(n), "version=6, mode=unshaped") {
		t.Fatal("v6 syntax lost")
	}
	for _, ua := range []string{"Surge iOS/5.19.0", "Surge Mac/6.6.0"} {
		if len(FilterSurgeSnell([]Node{n}, ua)) != 0 {
			t.Fatal("old/unknown Surge received v6", ua)
		}
	}
	for _, ua := range []string{"Surge iOS/5.20.0", "Surge Mac/6.7.0", "Mozilla/5.0", "Surge/unknown"} {
		if len(FilterSurgeSnell([]Node{n}, ua)) != 1 {
			t.Fatal("compatible/manual Surge export filtered", ua)
		}
	}
	n.SnellSinglePSK = false
	n.SnellUserKey = "singbox-only"
	if SurgeLine(n) != "" {
		t.Fatal("Surge received userkey-only mode")
	}
	n.SnellSinglePSK = true
	n.SnellPSK = "key\nInjected = direct"
	if SurgeLine(n) != "" {
		t.Fatal("source injected a policy")
	}
}
