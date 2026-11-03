package panel

import (
	"archive/zip"
	"bytes"
	"connectrpc.com/connect"
	"encoding/json"
	"fmt"
	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"gorm.io/gorm"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/backupfmt"
	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func websiteFixture(t *testing.T) (*Server, *apiClient, websiteRequest) {
	t.Helper()
	s, _, a := newAdminClient(t)
	cert, key, _ := pki.SelfSigned([]string{"site.example.com"}, 1e15)
	for _, row := range []any{
		&store.Node{ID: "web-node", Name: "web-node", AgentFeatures: store.NewJSON([]string{model.WebsiteFeature})},
		&store.Certificate{ID: "web-cert", Domain: "site.example.com", Provider: "manual", Cert: string(cert), Key: s.st.Box.Seal(string(key))},
		&store.CDNProfile{ID: "web-cdn", Name: "web-cdn", OriginDomain: "site.example.com", CaddyFronting: true, Ports: store.NewJSON([]int{443})},
		&store.Inbound{ID: "web-ws", NodeID: "web-node", Tag: "web-ws", Core: model.CoreXray, Port: 2053, Enabled: true, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransWS, Path: "/ws"}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecTLS, CertificateID: "web-cert", ServerName: "site.example.com"})},
		&store.Endpoint{ID: "web-ep", InboundID: "web-ws", Kind: "cdn", CDNProfileID: "web-cdn"},
	} {
		if err := s.st.DB.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	req := websiteRequest{Website: store.Website{NodeID: "web-node", Domain: "site.example.com", CertificateID: "web-cert", Enabled: true, Path: "/", Mode: "static", Files: store.NewJSON(map[string][]byte{"index.html": []byte("<h1>blog</h1>"), "css/app.css": []byte("body{}")})}}
	return s, a, req
}
func TestWebsiteAdmissionPublishBackup(t *testing.T) {
	s, a, req := websiteFixture(t)
	req.PanelUpstream = "http://127.0.0.1:8080"
	req.SubPath = "/sub"
	req.PortalPath = "/u"
	req.NodePaths = map[string]string{"web-ws": "/private-ws"}
	code, body := a.do("POST", "/websites/preview", req)
	if code != 200 {
		t.Fatal(code, body)
	}
	var preview websitePreview
	if err := json.Unmarshal(websiteTestJSON(body), &preview); err != nil {
		t.Fatal(err)
	}
	if len(preview.Errors) > 0 || len(preview.Routes) != 7 {
		t.Fatal(body)
	}
	code, body = a.do("POST", "/websites", req)
	if code != 200 {
		t.Fatal(code, body)
	}
	var response struct {
		Site store.Website `json:"site"`
	}
	json.Unmarshal(websiteTestJSON(body), &response)
	if response.Site.ID == "" || response.Site.Version != 1 || response.Site.Files.V != nil {
		t.Fatal("bad sanitized response", body)
	}
	var publishedNode store.Node
	s.st.DB.First(&publishedNode, "id = ?", "web-node")
	if publishedNode.DesiredRev == 0 {
		t.Fatal("save returned before publishing the desired revision")
	}
	var in store.Inbound
	s.st.DB.First(&in, "id = ?", "web-ws")
	if in.Transport.V.Path != "/private-ws" {
		t.Fatal("listener path not updated")
	}
	spec, err := s.BuildSpec("web-node")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Websites) != 1 || len(spec.Fronts) != 1 || !spec.HasCore(model.CoreCaddy) || spec.Fronts[0].Path != "/private-ws" || len(spec.Websites[0].Proxies) != 5 {
		t.Fatalf("bad combined spec: %+v", spec)
	}
	if string(spec.Websites[0].Files["index.html"]) != "<h1>blog</h1>" {
		t.Fatal("bundle not delivered")
	}
	if !contains(s.nodesUsingCert("web-cert"), "web-node") {
		t.Fatal("renewal did not include website node")
	}
	if code, _ := a.do("DELETE", "/certificates/web-cert", nil); code != 409 {
		t.Fatal("used website certificate deleted", code)
	}
	// An unmodified editor preserves the public bundle; an old version cannot overwrite it.
	req.Website = response.Site
	req.Mode = "proxy"
	req.Upstream = "http://127.0.0.1:3000"
	if code, body = a.do("POST", "/websites", req); code != 200 {
		t.Fatal(code, body)
	}
	if code, _ = a.do("POST", "/websites", req); code != 409 {
		t.Fatal("stale editor accepted")
	}
	var saved store.Website
	s.st.DB.First(&saved, "id = ?", req.ID)
	if len(saved.Files.V) != 2 {
		t.Fatal("editor erased bundle")
	}
	tables, _, err := backupfmt.ExportAll(s.st.DB)
	if err != nil {
		t.Fatal(err)
	}
	other := newTestServer(t)
	if err := other.st.DB.Transaction(func(tx *gorm.DB) error { return backupfmt.ImportAll(tx, tables, func(x string) string { return x }) }); err != nil {
		t.Fatal(err)
	}
	var restored store.Website
	if err := other.st.DB.First(&restored, "id = ?", req.ID).Error; err != nil {
		t.Fatal(err)
	}
	if string(restored.Files.V["css/app.css"]) != "body{}" || restored.CertificateID != "web-cert" {
		t.Fatal("website backup lost state")
	}
	if code, body = a.do("DELETE", "/websites/"+req.ID, nil); code != 204 {
		t.Fatal(code, body)
	}
	spec, err = s.BuildSpec("web-node")
	if err != nil || len(spec.Websites) != 0 || len(spec.Fronts) != 1 {
		t.Fatal("deletion changed existing CDN fronts", err)
	}
}
func TestWebsiteRejectsConflictsAndUnsafeTargets(t *testing.T) {
	s, a, base := websiteFixture(t)
	for _, change := range []func(*websiteRequest){
		func(r *websiteRequest) { r.Domain = "other.example.com" },
		func(r *websiteRequest) {
			r.Files = store.NewJSON(map[string][]byte{"../outside": []byte("x"), "index.html": []byte("x")})
		},
		func(r *websiteRequest) { r.Mode = "proxy"; r.Upstream = "http://localhost:3000" },
		func(r *websiteRequest) { r.Mode = "proxy"; r.Upstream = "http://10.0.0.1:3000" },
		func(r *websiteRequest) { r.Mode = "proxy"; r.Upstream = "http://127.0.0.1:443" },
		func(r *websiteRequest) {
			r.PanelUpstream = "http://127.0.0.1:8080"
			r.SubPath = "/sub"
			r.NodePaths = map[string]string{"web-ws": "/sub/nested"}
		},
		func(r *websiteRequest) { r.NodePaths = map[string]string{"unknown": "/ws"} },
		func(r *websiteRequest) { r.Path = "/ws" },
		func(r *websiteRequest) { r.NodePaths = map[string]string{"web-ws": "/"} },
	} {
		r := base
		change(&r)
		if code, body := a.do("POST", "/websites", r); code < 400 {
			t.Fatal("invalid website saved", body)
		}
	}
	s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "web-ep").Update("sni", "other.example.com")
	if code, body := a.do("POST", "/websites", base); code < 400 || !strings.Contains(fmt.Sprint(body), "SNI") {
		t.Fatal("uncovered entry SNI accepted", code, body)
	}
	s.st.DB.Model(&store.Endpoint{}).Where("id = ?", "web-ep").Update("sni", "")
	s.st.DB.Create(&store.Inbound{ID: "block443", NodeID: "web-node", Tag: "reality", Port: 443, Enabled: true, Protocol: store.NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS}), Transport: store.NewJSON(model.TransportSpec{Type: model.TransTCP}), Security: store.NewJSON(model.SecuritySpec{Type: model.SecReality})})
	if code, body := a.do("POST", "/websites", base); code < 400 || !strings.Contains(fmt.Sprint(body), "443") {
		t.Fatal(code, body)
	}
	// A request without an administrative credential cannot read or upload site data.
	req, _ := http.NewRequest("GET", a.base+"/websites", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatal(resp.StatusCode)
	}
}
func TestWebsiteArchiveBoundaries(t *testing.T) {
	_, a, _ := websiteFixture(t)
	upload := func(files map[string]string, symlink bool) (int, string) {
		t.Helper()
		var b bytes.Buffer
		w := zip.NewWriter(&b)
		for name, data := range files {
			h := &zip.FileHeader{Name: name, Method: zip.Deflate}
			if symlink {
				h.SetMode(os.ModeSymlink | 0777)
			}
			r, _ := w.CreateHeader(h)
			io.WriteString(r, data)
		}
		w.Close()
		req, _ := http.NewRequest("POST", a.base+"/websites/archive", &b)
		req.Header.Set("Authorization", "Bearer "+a.tok)
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out)
	}
	if code, body := upload(map[string]string{"index.html": "blog", "assets/main.js": "console.log('ok')"}, false); code != 200 || !strings.Contains(body, "fileCount") {
		t.Fatal(code, body)
	}
	for _, files := range []map[string]string{{"../escape": "x", "index.html": "x"}, {"/absolute": "x", "index.html": "x"}, {".env": "SECRET", "index.html": "x"}, {"folder/index.html": "x"}, {"index.html": strings.Repeat("a", model.MaxWebsiteBytes+1)}} {
		if code, _ := upload(files, false); code < 400 {
			t.Fatal("bad archive accepted")
		}
	}
	if code, _ := upload(map[string]string{"index.html": "link"}, true); code < 400 {
		t.Fatal("symlink accepted")
	}
}

func websiteTestJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestWebsiteRequiresCapableAgent(t *testing.T) {
	s, a, req := websiteFixture(t)
	if code, body := a.do("POST", "/websites", req); code != 200 {
		t.Fatal(code, body)
	}
	s.st.DB.Model(&store.Node{}).Where("id = ?", "web-node").Update("cert_serial", "1")
	rev, _, err := s.Publish("web-node", "website capability test", true)
	if err != nil {
		t.Fatal(err)
	}
	service := &agentService{s: s}
	request := connect.NewRequest(&polarisv1.GetSpecRequest{Rev: rev})
	if _, err = service.GetSpec(tunnelPeer("web-node"), request); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal("old Agent accepted a website spec", err)
	}
	request.Header().Set("X-Polaris-Agent-Features", model.WebsiteFeature)
	response, err := service.GetSpec(tunnelPeer("web-node"), request)
	if err != nil {
		t.Fatal(err)
	}
	var spec model.NodeSpec
	if err = json.Unmarshal(response.Msg.SpecJson, &spec); err != nil || len(spec.Websites) != 1 {
		t.Fatal("capable Agent lost site", err)
	}
	if string(spec.Websites[0].Files["index.html"]) != "<h1>blog</h1>" {
		t.Fatal("bundle missing from authenticated delivery")
	}
}
