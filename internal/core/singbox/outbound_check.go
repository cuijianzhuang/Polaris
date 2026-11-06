package singbox

import (
	"fmt"

	"github.com/cuijianzhuang/polaris/internal/model"
)

// CheckOutbound validates settings that sing-box cannot represent. Unsupported
// streams must fail rather than silently becoming ordinary TCP connections.
func CheckOutbound(o model.Outbound) error {
	s, err := o.ParseSettings()
	if err != nil {
		return err
	}
	return checkOutboundSettings(o.Type, s)
}

func checkOutboundSettings(typ model.OutboundType, s model.OutboundSettings) error {
	if typ == model.OutVLESS && s.Encryption != "" && s.Encryption != "none" {
		return fmt.Errorf("sing-box does not support VLESS Encryption")
	}
	if s.Transport == nil {
		return nil
	}
	t := s.Transport
	switch t.Type {
	case "", model.TransTCP:
		if t.HeaderType != "" && t.HeaderType != "none" {
			return fmt.Errorf("sing-box does not support TCP %s header camouflage", t.HeaderType)
		}
		return nil
	case model.TransQUIC:
		// QUIC is intrinsic to Hysteria2, rather than a pluggable stream.
		if typ == model.OutHysteria2 {
			return nil
		}
	case model.TransWS, model.TransGRPC, model.TransHTTPUpgrade:
		if typ == model.OutVLESS || typ == model.OutVMess || typ == model.OutTrojan {
			return nil
		}
	}
	return fmt.Errorf("sing-box does not support %s outbound transport for %s", t.Type, typ)
}
