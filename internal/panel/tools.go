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
	from := "panel"
	if req.NodeID != "" && s.hub.Online(req.NodeID) {
		res, err := s.hub.Command(c, req.NodeID, "reality_scan", map[string]string{"target": req.Target}, 60*time.Second)
		if err == nil && res.Ok {
			var rs []realityscan.Result
			// Agents before multi-target scans probe "a,b" as one host;
			// scan from the panel instead.
			if json.Unmarshal([]byte(res.Output), &rs) == nil && !(len(rs) == 1 && strings.ContainsAny(rs[0].Target, ", \n")) {
				c.JSON(http.StatusOK, gin.H{"results": rs, "from": "node"})
				return
			}
		} else if err == nil && res.Error != "" && !strings.HasPrefix(res.Error, "unknown command") {
			badRequest(c, fmt.Errorf("%s", res.Error))
			return
		}
		from = "panel (节点不支持扫描，请升级 Agent)"
	}
	rs, err := realityscan.Scan(c.Request.Context(), req.Target)
	if err != nil {
		badRequest(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"results": rs, "from": from})
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
