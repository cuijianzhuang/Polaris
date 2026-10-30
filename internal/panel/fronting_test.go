package panel

import (
	"slices"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

func TestCaddyFronting(t *testing.T) {
	s := newTestServer(t)
	db := s.st.DB
	db.Create(&store.Node{ID: "n1", Name: "jp", Address: "203.0.113.5"})
	db.Create(&store.Core{ID: "c1", NodeID: "n1", Type: model.CoreXray, Version: "26.3.27"})
	certPEM, keyPEM, _ := pki.SelfSigned([]string{"cdn.example.com"}, 1e15)
	db.Create(&store.Certificate{ID: "cert", Domain: "cdn.example.com", Provider: "manual", Cert: string(certPEM), Key: s.st.Box.Seal(string(keyPEM))})
	mk := func(id, tag string, port int, tr model.TransportSpec) {
		db.Create(&store.Inbound{ID: id, NodeID: "n1", Tag: tag, Port: port, Core: model.CoreXray, Enabled: true,
			Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(tr),
			Security: store.NewJSON(model.SecuritySpec{Type: model.SecTLS, ServerName: "cdn.example.com", CertificateID: "cert"})})
	}
	mk("ws", "ws", 2053, model.TransportSpec{Type: model.TransWS, Path: "/ws"})
	mk("grpc", "grpc", 2083, model.TransportSpec{Type: model.TransGRPC, ServiceName: "svc"})
	db.Create(&store.CDNProfile{ID: "cf", Name: "cf", OriginDomain: "cdn.example.com", CaddyFronting: true, Ports: store.NewJSON([]int{443})})
	for _, id := range []string{"ws", "grpc"} {
		db.Create(&store.Endpoint{ID: "ep-" + id, InboundID: id, Kind: "cdn", CDNProfileID: "cf"})
	}
	spec, err := s.BuildSpec("n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Fronts) != 2 || !spec.HasCore(model.CoreCaddy) {
		t.Fatalf("fronts: %+v", spec.Fronts)
	}
	for _, in := range spec.Inbounds {
		if in.Listen != "127.0.0.1" || in.Security.Type != model.SecNone || in.Port < frontInternalBase || !slices.Equal(in.Transport.TrustedForwardedFor, []string{"X-Polaris-Client-IP"}) {
			t.Fatalf("inbound not moved behind caddy: %+v", in)
		}
	}
	var grpc model.FrontRule
	for _, f := range spec.Fronts {
		if f.Inbound == "grpc" {
			grpc = f
		}
	}
	if grpc.Path != "/svc" || !grpc.H2C || grpc.Port != 443 || grpc.CertPEM == "" {
		t.Fatalf("grpc front: %+v", grpc)
	}
	// Subscription: the direct entry also goes through Caddy's port.
	db.Create(&store.User{ID: "u", Username: "u", UUID: "6f1d3c1c-7a0e-4a9f-9a52-6b8f6e4f1a01", Password: "p", Status: "active", SubToken: "tok-tok-tok-tok-tok"})
	db.Create(&store.Group{ID: "g", Name: "g"})
	db.Create(&store.UserGroup{UserID: "u", GroupID: "g"})
	db.Create(&store.GroupInbound{GroupID: "g", InboundID: "ws"})
	db.Create(&store.Endpoint{ID: "d", InboundID: "ws", Kind: "direct"})
	var u store.User
	db.First(&u, "id = ?", "u")
	nodes, _ := s.subNodes(&u)
	for _, n := range nodes {
		if n.Port != 443 || n.TLS.ServerName != "cdn.example.com" {
			t.Fatalf("fronted subscription node: %+v", n)
		}
	}
	// A REALITY inbound on 443 blocks fronting on that port.
	db.Create(&store.Inbound{ID: "r", NodeID: "n1", Tag: "reality", Port: 443, Core: model.CoreXray, Enabled: true,
		Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}),
		Security: store.NewJSON(model.SecuritySpec{Type: model.SecReality})})
	spec, _ = s.BuildSpec("n1")
	if len(spec.Fronts) != 0 {
		t.Fatal("fronting must be skipped when the public port is taken")
	}
	for _, in := range spec.Inbounds {
		if len(in.Transport.TrustedForwardedFor) != 0 && !slices.Equal(in.Transport.TrustedForwardedFor, []string{""}) {
			t.Fatal("blocked fronting retained trust", in.Tag)
		}
	}
	_ = sub.Node{}
}
