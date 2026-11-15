package panel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/cuijianzhuang/polaris/internal/license"
	"github.com/cuijianzhuang/polaris/internal/store"
	"github.com/gin-gonic/gin"
)

// panelAction is POST /panel/actions on a member panel: a hub runs one
// allow-listed write action through the panel:actions token scope. It
// dispatches to the existing handlers, so validation, auditing and error
// handling stay identical to the member's own admin UI.
func (s *Server) panelAction(c *gin.Context) {
	var req struct {
		Action       string  `json:"action" binding:"required"`
		NodeID       string  `json:"nodeId"`
		UserID       string  `json:"userId"`
		Days         int     `json:"days"`
		ResetTraffic bool    `json:"resetTraffic"`
		GB           float64 `json:"gb"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}

	switch req.Action {
	case "node.sync":
		if req.NodeID == "" {
			badRequest(c, errors.New("nodeId 必填"))
			return
		}
		c.Params = gin.Params{{Key: "id", Value: req.NodeID}}
		s.syncNode(c)
	case "node.upgrade-agent":
		if req.NodeID == "" {
			badRequest(c, errors.New("nodeId 必填"))
			return
		}
		c.Params = gin.Params{{Key: "id", Value: req.NodeID}}
		s.upgradeAgentAPI(c)
	case "user.renew":
		if req.UserID == "" {
			badRequest(c, errors.New("userId 必填"))
			return
		}
		c.Params = gin.Params{{Key: "id", Value: req.UserID}}
		rebindBody(c, map[string]any{"days": req.Days, "resetTraffic": req.ResetTraffic})
		s.renewUser(c)
	case "user.reset-traffic":
		if req.UserID == "" {
			badRequest(c, errors.New("userId 必填"))
			return
		}
		c.Params = gin.Params{{Key: "id", Value: req.UserID}}
		s.resetUserTraffic(c)
	case "user.add-traffic":
		if req.UserID == "" {
			badRequest(c, errors.New("userId 必填"))
			return
		}
		c.Params = gin.Params{{Key: "id", Value: req.UserID}}
		rebindBody(c, map[string]any{"gb": req.GB})
		s.addUserTraffic(c)
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "未知操作: " + req.Action})
	}
}

// rebindBody replaces the request body so a dispatched handler can bind its
// own struct after panelAction already consumed the original body.
func rebindBody(c *gin.Context, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		badRequest(c, err)
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(b))
	c.Request.ContentLength = int64(len(b))
}

// remotePanelAction is POST /panels/:id/actions on the hub: it forwards the
// action to the member and audits the operation on both sides.
func (s *Server) remotePanelAction(c *gin.Context) {
	if !s.requireFeature(c, license.FeatureMultiPanel) {
		return
	}
	id := c.Param("id")
	var p store.RemotePanel
	if err := s.st.DB.First(&p, "id = ?", id).Error; err != nil {
		dbError(c, err)
		return
	}

	token, err := s.st.Box.Open(p.Token)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "decrypt token"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 64<<10))
	if err != nil {
		badRequest(c, err)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		badRequest(c, errors.New("invalid JSON body"))
		return
	}
	action, _ := req["action"].(string)

	cli := &remotePanelClient{
		baseURL:  p.BaseURL,
		token:    token,
		caPin:    p.CAPin,
		insecure: p.Insecure,
		timeout:  remotePanelTimeout,
		rt:       s.remotePanelTransport,
	}

	var resp map[string]any
	status, err := cli.post(c.Request.Context(), "/api/panel/actions", req, &resp)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": classifyRemotePanelError(err)})
		return
	}

	s.auditCtx(c, "panel.action", p.ID, action)
	c.JSON(status, resp)
}
