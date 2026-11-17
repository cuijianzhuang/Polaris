package singbox

import (
	"encoding/json"
	"fmt"
	"strings"
)

// removedProxyProtocolKeys are PROXY-protocol option spellings that sing-box
// removed in 1.6.0. Any of them on a TCP listener makes the core abort at
// startup with "Proxy Protocol is deprecated and removed in sing-box 1.6.0".
// Polaris only supports sing-box >= 1.11, so none are ever valid.
var removedProxyProtocolKeys = []string{
	"proxy_protocol",
	"proxy_protocol_accept_no_header",
	"acceptProxyProtocol", // Xray-style key pasted into a sing-box Extra
}

// stripRemovedProxyProtocol removes obsolete PROXY-protocol keys from a raw
// Extra override before it is merged into a sing-box listener. This keeps a
// stale or hand-pasted configuration from crashing the core at startup. It is
// a recovery guard: new configs are rejected earlier by panel validation.
// Returns the Extra unchanged when it is empty, invalid or has no such keys.
func stripRemovedProxyProtocol(extra json.RawMessage) json.RawMessage {
	if len(extra) == 0 || !json.Valid(extra) {
		return extra
	}
	var obj map[string]any
	if err := json.Unmarshal(extra, &obj); err != nil {
		return extra
	}
	removed := false
	for _, key := range removedProxyProtocolKeys {
		if _, ok := obj[key]; ok {
			delete(obj, key)
			removed = true
		}
	}
	if !removed {
		return extra
	}
	cleaned, err := json.Marshal(obj)
	if err != nil {
		return extra
	}
	return cleaned
}

// CheckInboundExtra rejects a raw Extra override that would make sing-box
// abort at startup. Used by panel validation so operators get an actionable
// error at save time instead of a dead node after the next config push.
func CheckInboundExtra(extra json.RawMessage) error {
	if len(extra) == 0 || !json.Valid(extra) {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(extra, &obj); err != nil {
		return nil
	}
	for _, key := range removedProxyProtocolKeys {
		if _, ok := obj[key]; ok {
			return fmt.Errorf("sing-box 自 1.6.0 起已移除 PROXY protocol，Extra 中的 %s 会导致内核启动失败；请从 Extra 删除该字段（真实客户端 IP 请改用 Xray TCP 入站）", key)
		}
	}
	return nil
}

// hasRemovedProxyProtocol reports whether a raw Extra contains an obsolete key.
func hasRemovedProxyProtocol(extra json.RawMessage) bool {
	if len(extra) == 0 || !json.Valid(extra) {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(extra, &obj); err != nil {
		return false
	}
	for _, key := range removedProxyProtocolKeys {
		if _, ok := obj[key]; ok {
			return true
		}
	}
	return false
}

// describeRemovedProxyProtocol names the obsolete keys present in a raw Extra.
func describeRemovedProxyProtocol(extra json.RawMessage) string {
	var obj map[string]any
	if err := json.Unmarshal(extra, &obj); err != nil {
		return ""
	}
	var found []string
	for _, key := range removedProxyProtocolKeys {
		if _, ok := obj[key]; ok {
			found = append(found, key)
		}
	}
	return strings.Join(found, ", ")
}
