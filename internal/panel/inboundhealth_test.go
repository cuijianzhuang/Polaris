package panel

import (
	"net"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestInboundHealth(t *testing.T) {
	s, _, c := newAdminClient(t)
	ch := events(s)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	s.st.DB.Create(&store.Node{ID: "n1", Name: "tokyo", Address: "127.0.0.1", Status: "online"})
	s.st.DB.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27", InstalledVersions: store.NewJSON([]string{})})
	code, out := c.do("POST", "/inbounds", map[string]any{"nodeId": "n1", "core": "xray", "tag": "vless", "port": port,
		"protocol": map[string]any{"type": "vless"}, "transport": map[string]any{"type": "tcp"}, "security": map[string]any{"type": "none"}})
	if code != 201 {
		t.Fatalf("inbound %d %v", code, out)
	}
	id := out["inbound"].(map[string]any)["id"].(string)
	health := func() store.InboundHealth {
		var h store.InboundHealth
		s.st.DB.First(&h, "inbound_id = ?", id)
		return h
	}
	s.probeInbounds()
	if h := health(); !h.OK || h.CheckedAt.IsZero() {
		t.Fatalf("listening port: %+v", h)
	}
	if _, v := c.do("GET", "/inbounds/"+id, nil); v["health"].(map[string]any)["ok"] != true {
		t.Fatalf("view: %v", v["health"])
	}

	ln.Close() // now refused
	for i := 0; i < 3; i++ {
		s.probeInbounds()
	}
	if h := health(); h.OK || h.Fails != 3 || !strings.Contains(h.Error, "拒绝") {
		t.Fatalf("closed port: %+v", h)
	}
	if txt := next(t, ch); !strings.Contains(txt, "入站无法连接") || !strings.Contains(txt, "vless") {
		t.Fatalf("down alert: %q", txt)
	}
	s.probeInbounds() // no second alert while still down
	select {
	case e := <-ch:
		t.Fatalf("repeated alert: %q", e.Text)
	default:
	}

	ln2, err := net.Listen("tcp", ln.Addr().String())
	if err != nil {
		t.Skip("port taken again:", err)
	}
	defer ln2.Close()
	go func() {
		for {
			conn, err := ln2.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	if code, out := c.do("POST", "/inbounds/"+id+"/probe", nil); code != 200 || out["ok"] != true {
		t.Fatalf("manual probe %d %v", code, out)
	}
	if txt := next(t, ch); !strings.Contains(txt, "已恢复") {
		t.Fatalf("recovery alert: %q", txt)
	}

	// Offline nodes are skipped by the schedule.
	s.st.DB.Model(&store.Node{}).Where("id = ?", "n1").Update("status", "offline")
	before := health().CheckedAt
	s.probeInbounds()
	if !health().CheckedAt.Equal(before) {
		t.Fatal("probed an offline node")
	}
}
