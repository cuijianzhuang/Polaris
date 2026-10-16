package panel

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Bulk inbound operations: change ports (sequential from a start, shifted by
// an offset, or random, e.g. to move away from blocked ports), set a speed
// limit, enable or disable — over any selection, across nodes.

type bulkInboundReq struct {
	IDs    []string `json:"ids" binding:"required"`
	Action string   `json:"action" binding:"required"` // port / speed / enable / disable
	Mode   string   `json:"mode"`                      // port: sequential / offset / random
	Start  int      `json:"start"`                     // sequential: first port
	Offset int      `json:"offset"`                    // offset: added to each port
	Min    int      `json:"min"`                       // random: range
	Max    int      `json:"max"`
	Mbps   int      `json:"mbps"` // speed
}

type bulkResult struct {
	ID    string `json:"id"`
	Tag   string `json:"tag"`
	Node  string `json:"node"`
	Port  int    `json:"port,omitempty"`
	Error string `json:"error,omitempty"`
}

// bulkInbounds: POST /inbounds/bulk.
func (s *Server) bulkInbounds(c *gin.Context) {
	var req bulkInboundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err)
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 500 {
		badRequest(c, errors.New("请选择 1–500 个入站"))
		return
	}
	var ins []store.Inbound
	s.st.DB.Where("id IN ?", req.IDs).Order("node_id, port").Find(&ins)
	names := s.nodeNames()
	dirty := map[string]bool{}
	out := make([]bulkResult, 0, len(ins))
	switch req.Action {
	case "speed":
		if err := checkSpeedLimit(req.Mbps); err != nil {
			badRequest(c, err)
			return
		}
		for _, in := range ins {
			s.st.DB.Model(&store.Inbound{}).Where("id = ?", in.ID).Update("speed_limit_mbps", req.Mbps)
			dirty[in.NodeID] = true
			out = append(out, bulkResult{ID: in.ID, Tag: in.Tag, Node: names[in.NodeID]})
		}
	case "enable", "disable":
		for _, in := range ins {
			s.st.DB.Model(&store.Inbound{}).Where("id = ?", in.ID).Update("enabled", req.Action == "enable")
			dirty[in.NodeID] = true
			out = append(out, bulkResult{ID: in.ID, Tag: in.Tag, Node: names[in.NodeID]})
		}
	case "port":
		if req.Mode == "random" {
			req.Min, req.Max = max(req.Min, 1024), min(orInt(req.Max, 60000), 65535)
			if req.Max-req.Min < 100 {
				badRequest(c, errors.New("随机端口范围至少 100 个端口"))
				return
			}
		}
		for i, in := range ins {
			r := bulkResult{ID: in.ID, Tag: in.Tag, Node: names[in.NodeID]}
			port, err := s.bulkPort(&in, req, i)
			if err == nil {
				err = s.setInboundPort(&in, port)
			}
			if err != nil {
				r.Error = err.Error()
			} else {
				r.Port = port
				dirty[in.NodeID] = true
			}
			out = append(out, r)
		}
	default:
		badRequest(c, fmt.Errorf("未知操作 %q", req.Action))
		return
	}
	for n := range dirty {
		s.MarkDirty("inbounds bulk "+req.Action, n)
	}
	s.auditCtx(c, "inbound.bulk", req.Action, fmt.Sprintf("%d inbounds", len(out)))
	c.JSON(http.StatusOK, out)
}

func orInt(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}

// bulkPort picks the new port of the i-th selected inbound.
func (s *Server) bulkPort(in *store.Inbound, req bulkInboundReq, i int) (int, error) {
	m := s.toModelInbound(in)
	try := func(p int) error {
		if p < 1 || p > 65535 {
			return fmt.Errorf("端口 %d 超出范围", p)
		}
		m.Port = p
		return s.portConflict(in.NodeID, &m, in.ID)
	}
	switch req.Mode {
	case "sequential":
		p := req.Start + i
		return p, try(p)
	case "offset":
		p := in.Port + req.Offset
		return p, try(p)
	case "random":
		for n := 0; n < 50; n++ {
			p := req.Min + rand.IntN(req.Max-req.Min)
			if try(p) == nil {
				return p, nil
			}
		}
		return 0, errors.New("没有找到空闲端口")
	}
	return 0, fmt.Errorf("未知端口方式 %q", req.Mode)
}

// setInboundPort moves an inbound to port; a template-made inbound keeps
// the change (the field is marked as overridden).
func (s *Server) setInboundPort(in *store.Inbound, port int) error {
	upd := map[string]any{"port": port}
	if in.TemplateID != "" {
		upd["overridden_fields"] = store.NewJSON(mergeFields(in.OverriddenFields.V, []string{"port"}))
	}
	return s.st.DB.Model(&store.Inbound{}).Where("id = ?", in.ID).Updates(upd).Error
}
