package sub

import (
	"net/url"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func TestVisionUDP443ShareLinks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		enabled   bool
		transport model.Transport
		security  model.Security
		flow      string
		want      string
	}{
		{"default", false, model.TransTCP, model.SecReality, "xtls-rprx-vision", "xtls-rprx-vision"},
		{"reality", true, model.TransTCP, model.SecReality, "xtls-rprx-vision", "xtls-rprx-vision-udp443"},
		{"tls", true, model.TransTCP, model.SecTLS, "xtls-rprx-vision", "xtls-rprx-vision-udp443"},
		{"implicit tcp", true, "", model.SecTLS, "xtls-rprx-vision", "xtls-rprx-vision-udp443"},
		{"no vision", true, model.TransTCP, model.SecReality, "", ""},
		{"no tls", true, model.TransTCP, model.SecNone, "xtls-rprx-vision", "xtls-rprx-vision"},
		{"websocket", true, model.TransWS, model.SecTLS, "xtls-rprx-vision", "xtls-rprx-vision"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := sample()[0]
			n.Flow, n.VisionUDP443 = tc.flow, tc.enabled
			n.Transport.Type, n.TLS.Type = tc.transport, tc.security
			u, err := url.Parse(ShareLink(n))
			if err != nil {
				t.Fatal(err)
			}
			if got := u.Query().Get("flow"); got != tc.want {
				t.Fatalf("share-link flow = %q, want %q", got, tc.want)
			}
		})
	}
}

// Imported Xray links must remain usable when converted to native subscriptions.
func TestVisionUDP443ImportAndNativeClients(t *testing.T) {
	n := sample()[0]
	n.VisionUDP443 = true
	link := ShareLink(n)
	imported, err := ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Flow != "xtls-rprx-vision" || !imported.VisionUDP443 || ShareLink(imported) != link {
		t.Fatalf("lost UDP 443 preference on import: %+v", imported)
	}
	for _, entry := range []Node{n, imported} {
		for format, native := range map[string]map[string]any{"singbox": SingboxOutbound(entry), "mihomo": ClashProxy(entry)} {
			if native == nil || native["flow"] != "xtls-rprx-vision" {
				t.Fatalf("%s received incompatible Vision flow: %v", format, native)
			}
		}
	}
}
