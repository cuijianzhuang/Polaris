package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// TestTelegramLoginScope: a bot sign-in link is single-use; an operator's
// session sees and acts on only its own nodes, the owner's has full access.
func TestTelegramLoginScope(t *testing.T) {
	s, srv, admin := newAdminClient(t)
	for _, n := range []string{"mine", "other"} {
		if code, out := admin.do("POST", "/nodes", map[string]any{"name": n}); code != 201 {
			t.Fatalf("create node %d %v", code, out)
		}
	}
	var nodes []store.Node
	s.st.DB.Order("name").Find(&nodes) // mine, other
	mine, other := nodes[0].ID, nodes[1].ID
	op := store.TGAdmin{ID: "tgop", ChatID: 1001, Name: "alice", Role: "operator", NodeIDs: store.JSON[[]string]{V: []string{mine}}}
	owner := store.TGAdmin{ID: "tgown", ChatID: 1002, Name: "boss", Role: "owner"}
	s.st.DB.Create(&op)
	s.st.DB.Create(&owner)

	login := func(id string) *apiClient {
		c := &apiClient{t: t, base: srv.URL + s.cfg.APIBase()}
		link := s.tgLoginURL(s.newTGLoginCode(id))
		_, code, ok := strings.Cut(link, "#tg=")
		if !ok {
			t.Fatalf("link %s", link)
		}
		st, out := c.do("POST", "/auth/tg", map[string]string{"code": code})
		if st != 200 {
			t.Fatalf("tg login %d %v", st, out)
		}
		if st, _ := c.do("POST", "/auth/tg", map[string]string{"code": code}); st != 401 {
			t.Fatalf("code reuse must fail, got %d", st)
		}
		c.tok = out["token"].(string)
		return c
	}
	opc := login(op.ID)

	req, _ := http.NewRequest("GET", opc.base+"/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+opc.tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var list []map[string]any
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0]["id"] != mine {
		t.Fatalf("operator node list: %v", list)
	}
	expect := func(c *apiClient, method, path string, want int) {
		t.Helper()
		if st, out := c.do(method, path, nil); st != want {
			t.Fatalf("%s %s: %d %v, want %d", method, path, st, out, want)
		}
	}
	expect(opc, "GET", "/nodes/"+mine, 200)
	expect(opc, "GET", "/nodes/"+other, 403)
	expect(opc, "GET", "/nodes/"+other+"/revisions", 403)
	expect(opc, "POST", "/nodes/"+other+"/sync", 403)
	expect(opc, "PUT", "/nodes/"+mine, 403)           // editing is not an operator action
	expect(opc, "GET", "/nodes/"+mine+"/render", 403) // native config holds keys
	expect(opc, "GET", "/users", 403)
	expect(opc, "GET", "/users/any/traffic-sources", 403)
	expect(opc, "GET", "/settings", 403)
	expect(opc, "GET", "/stats/traffic?node="+mine, 200)
	expect(opc, "GET", "/stats/traffic?node="+other, 403)
	expect(opc, "GET", "/stats/traffic", 403)
	expect(opc, "GET", "/stats/traffic?node="+mine+"&user=x", 403)
	expect(opc, "GET", "/stats/overview", 403)
	if st, out := opc.do("GET", "/auth/me", nil); st != 200 || out["role"] != "operator" {
		t.Fatalf("me: %d %v", st, out)
	}
	// Removing the Telegram admin ends the session.
	s.st.DB.Delete(&op)
	expect(opc, "GET", "/nodes/"+mine, 401)

	own := login(owner.ID)
	expect(own, "GET", "/users", 200)
	expect(own, "GET", "/nodes/"+other, 200)
	expect(own, "GET", "/stats/overview", 200)
	if st, _ := (&apiClient{t: t, base: own.base}).do("POST", "/auth/tg", map[string]string{"code": "nope"}); st != 401 {
		t.Fatal("unknown code accepted")
	}
}

// TestTelegramLoginLinkUsesPanelAddress: with a separate subscription domain
// the bot's sign-in link still points at the panel, not the subscription host
// (which serves only subscriptions and would show the decoy).
func TestTelegramLoginLinkUsesPanelAddress(t *testing.T) {
	s, _, _ := newAdminClient(t)
	s.cfg.PublicURL = "https://panel.example.com"
	s.st.SetSetting("sub_base_url", "https://sub.example.com")
	link := s.tgLoginURL("abc")
	if !strings.HasPrefix(link, "https://panel.example.com") {
		t.Fatalf("login link %s", link)
	}
	if b := s.subBase(); b != "https://sub.example.com" {
		t.Fatalf("subBase %s", b)
	}
}
