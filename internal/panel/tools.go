package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/pki"
	"github.com/cuijianzhuang/polaris/internal/realityscan"
)

// realityScan is POST /tools/reality-scan {target, nodeId}: probes REALITY
// targets from the node (latency and reachability as the node sees them)
// when it is online, otherwise from the panel.
func (s *Server) realityScan(c *gin.Context) {
	var req struct {
		Target string `json:"target"`
		NodeID string `json:"nodeId"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if strings.TrimSpace(req.Target) == "" {
		// The candidate list is editable (设置 or the scanner dialog).
		req.Target = strings.TrimSpace(s.st.GetSetting("reality_scan_candidates", ""))
	}
	rs, from, err := s.scanTargets(c, req.Target, req.NodeID)
	if err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": rs, "from": from})
}

// scanTargets probes targets from the node when it is online and supports
// it, else from the panel.
func (s *Server) scanTargets(c *gin.Context, target, nodeID string) ([]realityscan.Result, string, error) {
	from := "panel"
	if nodeID != "" && s.hub.Online(nodeID) {
		res, err := s.hub.Command(c, nodeID, "reality_scan", map[string]string{"target": target}, 90*time.Second)
		if err == nil && res.Ok {
			var rs []realityscan.Result
			// Agents before multi-target scans probe "a,b" as one host;
			// scan from the panel instead.
			if json.Unmarshal([]byte(res.Output), &rs) == nil && !(len(rs) == 1 && strings.ContainsAny(rs[0].Target, ", \n")) {
				return rs, "node", nil
			}
		} else if err == nil && res.Error != "" && !strings.HasPrefix(res.Error, "unknown command") {
			return nil, "", fmt.Errorf("%s", res.Error)
		}
		from = "panel (节点不支持扫描，请升级 Agent)"
	}
	rs, err := realityscan.Scan(c.Request.Context(), target)
	return rs, from, err
}

// realityDiscover is POST /tools/reality-discover {query, country, pages,
// nodeId}: finds candidates in Shodan's index with the saved API key, then
// checks them like a normal scan (TLS 1.3, h2, certificate).
func (s *Server) realityDiscover(c *gin.Context) {
	var req struct {
		Query   string `json:"query"`
		Country string `json:"country"`
		Pages   int    `json:"pages"`
		NodeID  string `json:"nodeId"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	key := s.st.Box.MustOpen(s.st.GetSetting("shodan_api_key", ""))
	found, err := realityscan.ShodanDiscover(c.Request.Context(), key, realityscan.ShodanQuery{Query: req.Query, Country: req.Country, Pages: req.Pages})
	if err != nil {
		badRequest(c, err)
		return
	}
	out := gin.H{"query": found.Query, "total": found.Total, "candidates": found.Hosts, "results": []realityscan.Result{}, "from": ""}
	if len(found.Hosts) > 0 {
		rs, from, err := s.scanTargets(c, strings.Join(found.Hosts, ","), req.NodeID)
		if err != nil {
			badRequest(c, err)
			return
		}
		out["results"], out["from"] = rs, from
	}
	s.auditCtx(c, "tools.reality_discover", found.Query, fmt.Sprintf("%d candidates", len(found.Hosts)))
	c.JSON(http.StatusOK, out)
}

// shodanInfo is GET /tools/shodan: whether a key is saved and, if so, the
// query credits left.
func (s *Server) shodanInfo(c *gin.Context) {
	key := s.st.Box.MustOpen(s.st.GetSetting("shodan_api_key", ""))
	if key == "" {
		c.JSON(http.StatusOK, gin.H{"configured": false, "defaultQuery": realityscan.DefaultShodanQuery})
		return
	}
	credits, plan, err := realityscan.ShodanCredits(c.Request.Context(), key)
	out := gin.H{"configured": true, "defaultQuery": realityscan.DefaultShodanQuery, "credits": credits, "plan": plan}
	if err != nil {
		out["error"] = err.Error()
	}
	c.JSON(http.StatusOK, out)
}

// realityCandidates is GET /tools/reality-candidates: the targets an empty
// scan probes (the setting, else the built-in list).
func (s *Server) realityCandidates(c *gin.Context) {
	v := strings.TrimSpace(s.st.GetSetting("reality_scan_candidates", ""))
	c.JSON(http.StatusOK, gin.H{"candidates": v, "default": strings.Join(realityscan.Candidates, "\n")})
}

// genVLESSEnc is POST /tools/vlessenc {auth: x25519 | mlkem768}.
func (s *Server) genVLESSEnc(c *gin.Context) {
	var req struct {
		Auth string `json:"auth"`
	}
	_ = c.ShouldBindJSON(&req)
	d, e, err := pki.VLESSEncPair(req.Auth)
	if err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"decryption": d, "encryption": e})
}

// genMLDSA65 is POST /tools/mldsa65: a REALITY post-quantum signing seed.
func (s *Server) genMLDSA65(c *gin.Context) {
	seed, verify, err := pki.MLDSA65Pair()
	if err != nil {
		serverError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"seed": seed, "verify": verify})
}
