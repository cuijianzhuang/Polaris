package panel

import (
	"encoding/json"
	"testing"

	polarisv1 "github.com/cuijianzhuang/polaris/api/gen/polaris/v1"
	"github.com/cuijianzhuang/polaris/internal/realityscan"
	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestTemplateRealityNodeVerification(t *testing.T) {
	for _, mode := range []string{"offline", "unsupported", "malformed", "missing", "passed"} {
		t.Run(mode, func(t *testing.T) {
			s, c, id, opt := templateFixture(t)
			opt["nodeIds"], opt["verifyReality"] = []string{"n1"}, true
			opt["realityOverrides"] = map[string]map[string]templateRealityTarget{"n1": {"r": {Dest: "edge.example.com:443", ServerNames: []string{"sni.example.com"}}}}
			preview := previewFixture(t, c, id, opt)
			runID := preview["planId"].(string)
			opt["planId"] = runID
			if code, _ := c.do("POST", "/templates/"+id+"/apply", opt); code != 409 {
				t.Fatal("unchecked target applied")
			}
			if mode != "offline" {
				conn := s.hub.register("n1")
				t.Cleanup(func() { s.hub.unregister(conn) })
				go func() {
					select {
					case message := <-conn.out:
						cmd := message.GetCommand()
						var candidates []realityscan.Candidate
						if cmd.Kind != "reality_scan_v2" || json.Unmarshal([]byte(cmd.Args["candidates"]), &candidates) != nil || len(candidates) != 1 || candidates[0].Address != "edge.example.com:443" || candidates[0].ServerName != "sni.example.com" {
							t.Error("wrong node target/SNI command")
						}
						result := &polarisv1.CommandResult{Id: cmd.Id, Ok: true}
						switch mode {
						case "unsupported":
							result.Ok, result.Error = false, "unknown command: reality_scan_v2"
						case "malformed":
							result.Output = "null"
						case "missing":
							result.Output = "[]"
						default:
							result.Output = `[{"target":"edge.example.com:443","serverName":"sni.example.com","ok":true,"tls":"1.3","alpn":"h2"}]`
						}
						s.hub.resolve("n1", result)
					case <-t.Context().Done():
					}
				}()
			}
			code, check := c.do("POST", "/templates/"+id+"/runs/"+runID+"/reality-check", map[string]string{"nodeId": "n1"})
			if mode != "passed" && mode != "missing" {
				if code != 400 {
					t.Fatalf("invalid source accepted: %d %v", code, check)
				}
				return
			}
			if code != 200 || check["from"] != "node" || check["ok"] != (mode == "passed") {
				t.Fatalf("check: %d %v", code, check)
			}
			if mode == "missing" {
				if code, _ := c.do("POST", "/templates/"+id+"/apply", opt); code != 409 {
					t.Fatal("empty results accepted as success")
				}
				return
			}
			if code, out := c.do("POST", "/templates/"+id+"/apply", opt); code != 200 {
				t.Fatalf("apply verified plan: %d %v", code, out)
			}
			var in store.Inbound
			s.st.DB.Where("template_id = ?", id).First(&in)
			if in.Security.V.RealityDest != "edge.example.com:443" || !contains(in.OverriddenFields.V, "security.realityDest") || !contains(in.OverriddenFields.V, "security.realityServerNames") {
				t.Fatal("node-specific target was not preserved as an override")
			}
		})
	}
}
