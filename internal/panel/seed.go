package panel

import (
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/cuijianzhuang/polaris/internal/sub"
)

// Default system settings.
var defaultSettings = map[string]string{
	"site_title":          "Polaris",
	"sub_name_template":   "{flag} {node} {protocol}",
	"sub_update_interval": "12",
	"ip_ban_minutes":      "0",
	"acme_email":          "",
	"default_xray":        "26.3.27",
	"default_singbox":     "1.13.5",
}

func (s *Server) seed() error {
	for k, v := range defaultSettings {
		var n int64
		s.st.DB.Model(&store.Setting{}).Where("key = ?", k).Count(&n)
		if n == 0 {
			if err := s.st.SetSetting(k, v); err != nil {
				return err
			}
		}
	}
	var admins int64
	s.st.DB.Model(&store.Admin{}).Count(&admins)
	if admins == 0 {
		user, pass := s.cfg.InitialAdmin.Username, s.cfg.InitialAdmin.Password
		if user == "" {
			user = "admin"
		}
		generated := false
		if pass == "" {
			pass = pki.Password(16)
			generated = true
		}
		h, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if err := s.st.DB.Create(&store.Admin{ID: store.NewID(), Username: user, PasswordHash: string(h)}).Error; err != nil {
			return err
		}
		if generated {
			s.log.Warn("created initial admin; change the password after first login", "username", user, "password", pass)
		} else {
			s.log.Info("created initial admin", "username", user)
		}
	}
	for _, f := range []struct{ format, content string }{
		{"clash", sub.DefaultClashTemplate}, {"singbox", sub.DefaultSingboxTemplate}, {"surge", sub.DefaultSurgeTemplate},
	} {
		var n int64
		s.st.DB.Model(&store.SubTemplate{}).Where("format = ?", f.format).Count(&n)
		if n == 0 {
			s.st.DB.Create(&store.SubTemplate{ID: store.NewID(), Format: f.format, Name: "默认", Content: f.content, IsDefault: true, UpdatedAt: time.Now()})
		}
	}
	for _, t := range builtinTemplates() {
		var n int64
		s.st.DB.Model(&store.InboundTemplate{}).Where("name = ?", t.Name).Count(&n)
		if n == 0 {
			t.ID = store.NewID()
			if err := s.st.DB.Create(&t).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func builtinTemplates() []store.InboundTemplate {
	reality := store.TemplateItem{
		Key: "reality", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"},
		Transport:  model.TransportSpec{Type: model.TransTCP},
		Security:   model.SecuritySpec{Type: model.SecReality, RealityDest: "www.microsoft.com:443", RealityServerNames: []string{"www.microsoft.com"}},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       store.PortPolicy{Mode: "fixed", Fixed: 443},
		TagPattern: "{node.name}-vless-reality", Cert: "reality",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	rnd := store.PortPolicy{Mode: "range", Min: 20000, Max: 30000}
	hy2 := store.TemplateItem{
		Key: "hy2", Core: model.CoreAuto,
		Protocol:  model.ProtocolSpec{Type: model.ProtoHysteria2},
		Transport: model.TransportSpec{Type: model.TransQUIC},
		Security:  model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}", ALPN: []string{"h3"}},
		Port:      rnd, TagPattern: "{node.name}-hy2", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	tuic := hy2
	tuic.Key, tuic.Protocol, tuic.TagPattern = "tuic", model.ProtocolSpec{Type: model.ProtoTUIC, CongestionControl: "bbr"}, "{node.name}-tuic"
	anytls := store.TemplateItem{
		Key: "anytls", Core: model.CoreAuto,
		Protocol:  model.ProtocolSpec{Type: model.ProtoAnyTLS},
		Transport: model.TransportSpec{Type: model.TransTCP},
		Security:  model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}"},
		Port:      rnd, TagPattern: "{node.name}-anytls", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}},
	}
	xhttp := store.TemplateItem{
		Key: "xhttp", Core: model.CoreAuto,
		Protocol:  model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport: model.TransportSpec{Type: model.TransXHTTP, Path: "/{rand.path}", Mode: "auto"},
		Security:  model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}", ALPN: []string{"h2", "http/1.1"}},
		Sniffing:  model.SniffSpec{Enabled: true},
		Port:      rnd, TagPattern: "{node.name}-vless-xhttp", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "direct"}, {Kind: "cdn"}},
	}
	cdnWS := store.TemplateItem{
		Key: "cdn-ws", Core: model.CoreAuto,
		Protocol:   model.ProtocolSpec{Type: model.ProtoVLESS},
		Transport:  model.TransportSpec{Type: model.TransWS, Path: "/{rand.path}"},
		Security:   model.SecuritySpec{Type: model.SecTLS, ServerName: "{node.domain}"},
		Sniffing:   model.SniffSpec{Enabled: true},
		Port:       store.PortPolicy{Mode: "fixed", Fixed: 2053},
		TagPattern: "{node.name}-vless-ws", Cert: "acme",
		Endpoints: []store.EndpointTemplate{{Kind: "cdn"}},
	}
	mk := func(name, desc string, items ...store.TemplateItem) store.InboundTemplate {
		return store.InboundTemplate{Name: name, Builtin: true, Description: desc, Items: store.NewJSON(items),
			Groups: store.NewJSON([]string{}), BoundLabels: store.NewJSON([]string{})}
	}
	return []store.InboundTemplate{
		mk("REALITY 直连", "VLESS + Vision + REALITY，固定 443", reality),
		mk("Hy2 + TUIC", "Hysteria2 与 TUIC v5，随机高位端口", hy2, tuic),
		mk("CDN WS 备用", "VLESS + WebSocket + TLS，走 CDN 入口", cdnWS),
		mk("全协议", "REALITY、XHTTP、Hysteria2、TUIC、AnyTLS", reality, xhttp, hy2, tuic, anytls),
	}
}
