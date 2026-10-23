package panel

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/cuijianzhuang/polaris/internal/core"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

var templateVariableRE = regexp.MustCompile(`\{([^{}]+)\}`)
var templateVariables = map[string]bool{
	"node.name": true, "node.region": true, "node.domain": true, "node.ip": true,
	"protocol": true, "rand.path": true, "rand.shortid": true, "rand.port": true,
}

func checkTemplateVariables(value string) error {
	for _, match := range templateVariableRE.FindAllStringSubmatch(value, -1) {
		if !templateVariables[match[1]] {
			return fmt.Errorf("未知变量 {%s}", match[1])
		}
	}
	if rest := templateVariableRE.ReplaceAllString(value, ""); strings.ContainsAny(rest, "{}") {
		return fmt.Errorf("模板变量的大括号不完整")
	}
	return nil
}

// Validate before persistence, without installing cores or issuing certificates.
func (s *Server) validateTemplate(req *templateReq) error {
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > 64 {
		return fmt.Errorf("模板名称需为 1–64 个字符")
	}
	if utf8.RuneCountInString(req.Description) > 255 {
		return fmt.Errorf("模板说明不能超过 255 个字符")
	}
	if len(req.Items) == 0 || len(req.Items) > 100 {
		return fmt.Errorf("每个模板需包含 1–100 个入站")
	}
	seen := map[string]bool{}
	for i := range req.Items {
		it := &req.Items[i]
		fail := func(err error) error { return fmt.Errorf("第 %d 个入站（%s）：%w", i+1, it.Key, err) }
		if it.Key == "" || len(it.Key) > 64 || seen[it.Key] {
			return fail(fmt.Errorf("条目标识为空、过长或重复"))
		}
		seen[it.Key] = true
		if utf8.RuneCountInString(it.Name) > 64 {
			return fail(fmt.Errorf("入站名称不能超过 64 个字符"))
		}
		if err := s.validateTemplateItem(it); err != nil {
			return fail(err)
		}
	}
	for _, id := range req.Groups {
		var n int64
		if err := s.st.DB.Model(&store.Group{}).Where("id = ?", id).Count(&n).Error; err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("授权分组不存在：%s", id)
		}
	}
	return nil
}

func (s *Server) validateTemplateItem(it *store.TemplateItem) error {
	switch it.Port.Mode {
	case "fixed":
		if it.Port.Fixed < 1 || it.Port.Fixed > 65535 {
			return fmt.Errorf("固定端口必须在 1–65535 之间")
		}
	case "range":
		if it.Port.Min < 1 || it.Port.Max > 65535 || it.Port.Min > it.Port.Max {
			return fmt.Errorf("随机端口区间必须在 1–65535 之间，且起始端口不能大于结束端口")
		}
	default:
		return fmt.Errorf("端口策略必须为 fixed 或 range")
	}
	values := []string{it.TagPattern, it.Transport.Path, it.Transport.Host, it.Transport.ServiceName,
		it.Security.ServerName, it.Security.RealityDest}
	values = append(values, it.Security.RealityServerNames...)
	values = append(values, it.Security.RealityShortIDs...)
	for _, ep := range it.Endpoints {
		values = append(values, ep.Address)
	}
	for _, value := range values {
		if err := checkTemplateVariables(value); err != nil {
			return err
		}
	}
	if it.TagPattern == "" {
		it.TagPattern = "{node.name}-{protocol}"
	}
	m := model.Inbound{Tag: "check", Port: 1, Core: it.Core, Protocol: it.Protocol, Transport: it.Transport,
		Security: it.Security, Sniffing: it.Sniffing, OutboundIPStrategy: it.OutboundIPStrategy}
	// Templates may store only an authentication marker. As in planNode,
	// validate using a generated pair without persisting or sharing it.
	if auth := pki.VLESSAuthOf(m.Protocol.Decryption); auth != "" {
		var err error
		m.Protocol.Decryption, m.Protocol.Encryption, err = pki.VLESSEncPair(auth)
		if err != nil {
			return err
		}
	}
	plainEncryption := (m.Security.Type == model.SecNone || m.Security.Type == "") && m.Protocol.Decryption != "" && m.Protocol.Decryption != "none" && m.Protocol.Encryption != "" && m.Protocol.Encryption != "none"
	if m.Protocol.Flow != "" && (m.Protocol.Flow != "xtls-rprx-vision" || m.Protocol.Type != model.ProtoVLESS ||
		(m.Transport.Type != "" && m.Transport.Type != model.TransTCP) || (!plainEncryption && m.Security.Type != model.SecTLS && m.Security.Type != model.SecReality)) {
		return fmt.Errorf("Vision 流控需要 VLESS + TCP，以及 TLS/REALITY 或 VLESS Encryption；udp443 请使用订阅客户端开关")
	}
	m.Normalize()
	if m.Security.Type == model.SecReality {
		if m.Security.RealityDest == "" {
			return fmt.Errorf("REALITY 目标不能为空")
		}
		if len(m.Security.RealityServerNames) == 0 {
			m.Security.RealityServerNames = []string{"example.com"} // generated from the target at apply time
		}
	}
	if err := m.Validate(); err != nil {
		return err
	}
	if err := prepareXrayExtras(&m); err != nil {
		return err
	}
	cores := []model.CoreType{m.Core}
	if m.Core == "" || m.Core == model.CoreAuto {
		cores = []model.CoreType{model.CoreXray, model.CoreSingbox, model.CoreMihomo, model.CoreHysteria, model.CoreGost}
	}
	supported := false
	for _, ct := range cores {
		if caps, ok := core.Capabilities(ct, "999.0.0"); ok && caps.SupportsInbound(&m) == nil {
			supported = true
			break
		}
	}
	if !supported {
		return fmt.Errorf("所选内核不支持此协议、传输、安全或功能组合")
	}
	if it.Cert == "" {
		switch m.Security.Type {
		case model.SecReality:
			it.Cert = "reality"
		case model.SecTLS:
			it.Cert = "acme"
		default:
			it.Cert = "none"
		}
	}
	if m.Security.Type == model.SecTLS {
		if it.Cert != "acme" && it.Cert != "selfsigned" && !strings.HasPrefix(it.Cert, "wildcard:") {
			return fmt.Errorf("TLS 需选择 ACME、自签或已有证书策略")
		}
	} else if m.Security.Type == model.SecReality && it.Cert != "reality" {
		return fmt.Errorf("REALITY 应使用无需证书策略")
	}
	if strings.HasPrefix(it.Cert, "wildcard:") {
		var n int64
		s.st.DB.Model(&store.Certificate{}).Where("id = ?", strings.TrimPrefix(it.Cert, "wildcard:")).Count(&n)
		if n != 1 {
			return fmt.Errorf("引用的证书不存在")
		}
	}
	for _, ep := range it.Endpoints {
		if ep.Port < 0 || ep.Port > 65535 {
			return fmt.Errorf("入口端口必须在 1–65535 之间或留空")
		}
		switch ep.Kind {
		case "direct":
		case "cdn":
			if !m.CDNCapable() {
				return fmt.Errorf("此协议组合不支持 CDN 入口")
			}
			if ep.CDNProfileID != "" {
				var n int64
				s.st.DB.Model(&store.CDNProfile{}).Where("id = ?", ep.CDNProfileID).Count(&n)
				if n != 1 {
					return fmt.Errorf("CDN 配置档不存在")
				}
			}
		case "relay":
			var n int64
			s.st.DB.Model(&store.Chain{}).Where("id = ?", ep.RelayChainID).Count(&n)
			if ep.RelayChainID == "" || n != 1 {
				return fmt.Errorf("中转链不存在")
			}
		default:
			return fmt.Errorf("未知入口类型：%s", ep.Kind)
		}
	}
	return nil
}
