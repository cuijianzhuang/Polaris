package panel

import (
	"slices"
	"testing"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A public listener cannot distinguish genuine CDN headers from a direct
// client's forgery. A CDN entry alone must never enable forwarding trust.
func TestPublicCloudflareInboundDoesNotTrustForwardedFor(t *testing.T) {
	s, admin := cdnFixture(t)
	code, out := admin.do("POST", "/inbounds", grpcInboundReq(8443, map[string]any{"tag": "ws", "transport": map[string]any{"type": "ws", "path": "/x"}, "cdnProfileId": "cf"}))
	if code != 201 {
		t.Fatalf("cdn create: %d %v", code, out)
	}
	cdnID := out["inbound"].(map[string]any)["id"].(string)
	code, out = admin.do("POST", "/inbounds", grpcInboundReq(2083, map[string]any{"tag": "plain", "transport": map[string]any{"type": "ws", "path": "/y"}}))
	if code != 201 {
		t.Fatalf("plain create: %d %v", code, out)
	}
	spec, err := s.BuildSpec("n1")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, in := range spec.Inbounds {
		switch in.Tag {
		case "ws":
			seen++
			if in.ID != cdnID || !slices.Equal(in.Transport.TrustedForwardedFor, []string{""}) {
				t.Fatalf("cdn inbound: %+v", in.Transport)
			}
		case "plain":
			seen++
			if !slices.Equal(in.Transport.TrustedForwardedFor, []string{""}) {
				t.Fatalf("direct inbound trusts headers: %+v", in.Transport)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("inbounds in spec: %+v", spec.Inbounds)
	}
}

// Cloudflare edge addresses never count against a user's IP limit.
func TestCloudflareEdgesExemptFromLimit(t *testing.T) {
	s := newTestServer(t)
	s.st.SetSetting("ip_ban_minutes", "10")
	u := store.User{ID: store.NewID(), Username: "cf", UUID: store.NewID(), Password: "p", SubToken: store.NewID(), Status: "active", IPLimit: 1, OnlineIPs: store.NewJSON([]string{})}
	s.st.DB.Create(&u)
	n := store.Node{ID: store.NewID(), Name: "n", DefaultCore: model.CoreXray}
	s.st.DB.Create(&n)
	s.nodeUserSets.mu.Lock()
	s.nodeUserSets.m = map[string]nodeUsersEntry{n.ID: {set: map[string]bool{u.ID: true}, own: map[string]bool{u.ID: true}}}
	s.nodeUserSets.mu.Unlock()
	edges := []string{"172.69.33.48", "104.22.49.116", "104.22.193.81", "2606:4700::1"}
	s.handleOnlineIPs(n.ID, &polarisv1.OnlineIPs{Users: []*polarisv1.UserIPs{{UserId: u.ID, Ips: append(edges, "198.51.100.1")}}})
	var got store.User
	s.st.DB.First(&got, "id = ?", u.ID)
	if got.BannedUntil != nil {
		t.Fatal("Cloudflare edges counted against the IP limit")
	}
	s.handleOnlineIPs(n.ID, &polarisv1.OnlineIPs{Users: []*polarisv1.UserIPs{{UserId: u.ID, Ips: []string{"198.51.100.1", "198.51.100.2"}}}})
	s.st.DB.First(&got, "id = ?", u.ID)
	if got.BannedUntil == nil {
		t.Fatal("two device addresses over a limit of 1 not banned")
	}
}
