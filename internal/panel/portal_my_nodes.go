package panel

import (
	_ "embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// The user center's "我的节点" page (design user-nodes §9, batch 3): a
// server-rendered section page in the portal's existing style (compare
// portal_forwards.go) whose buttons call the /u/my-nodes JSON APIs from
// batches 1 and 3. The entry link is only added to the user center while
// the feature is on (see portalPage); the page itself stays reachable so
// existing nodes remain manageable while enrolment is off.

type portalMyNodeRow struct {
	ID, DisplayName, Address, Region, Flag, LabelsText string
	Online, Enrolled                                   bool
	ReviewState, ReviewText                            string
	Pending, Rejected, Approved                        bool
	AgentVersion                                       string
	Hostname, OS, CPUText, MemText, LastSeenText       string
	Inbounds                                           []portalInboundView
}

type portalMyNodesData struct {
	Off, CanAdd, Unlimited bool
	Mode                   string
	Limit                  int
	Count                  int64
	Nodes                  []portalMyNodeRow
	// EmptyInbound feeds the add-inbound form (the field template expects
	// an inbound view; a zero one renders blank fields).
	EmptyInbound portalInboundView
}

// portalMyNodesPage is GET /u/my-nodes-page.
func (s *Server) portalMyNodesPage(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	u := s.portalUser(c)
	if u == nil {
		c.Redirect(http.StatusFound, portalPath)
		return
	}
	mode := s.effectiveUserNodesMode()
	limit := s.userNodeCap(u)
	count := s.userNodeCount(u.ID)
	data := portalMyNodesData{Mode: mode, Off: mode == userNodesOff, Limit: limit, Count: count,
		Unlimited: limit <= 0, CanAdd: mode != userNodesOff && (limit <= 0 || count < int64(limit)),
		EmptyInbound: portalInboundView{Protocol: map[string]any{}, Transport: map[string]any{},
			Security: map[string]any{}, Sniffing: map[string]any{}}}

	var ns []store.Node
	s.st.DB.Where("owner_id = ?", u.ID).Order("created_at, id").Find(&ns)
	for i := range ns {
		n := &ns[i]
		v := s.portalNodeViewOf(n)
		row := portalMyNodeRow{ID: n.ID, DisplayName: v.DisplayName, Address: v.Address, Region: v.Region,
			Flag: v.Flag, LabelsText: strings.Join(v.Labels, ", "), Online: v.Connected, Enrolled: v.Enrolled,
			ReviewState: n.ReviewState, AgentVersion: v.AgentVersion,
			Hostname: v.System.Hostname, OS: v.System.OS}
		switch n.ReviewState {
		case "pending":
			row.Pending, row.ReviewText = true, "待审批"
		case "approved":
			row.Approved, row.ReviewText = true, "已通过"
		case "rejected":
			row.Rejected, row.ReviewText = true, "已拒绝"
		}
		if v.System.CPU > 0 || v.Enrolled {
			row.CPUText = fmt.Sprintf("%.0f%%", v.System.CPU)
			row.MemText = fmt.Sprintf("%.0f%%", v.System.Mem)
		}
		if v.LastSeen != nil {
			row.LastSeenText = v.LastSeen.Local().Format("01-02 15:04")
		}
		if row.Approved {
			var ins []store.Inbound
			if err := s.st.DB.Where("node_id = ?", n.ID).Order("port, tag").Find(&ins).Error; err == nil {
				for j := range ins {
					row.Inbounds = append(row.Inbounds, s.portalInboundViewOf(&ins[j]))
				}
			}
		}
		data.Nodes = append(data.Nodes, row)
	}
	s.renderPortalSection(c, http.StatusOK, "account", "我的节点", portalMyNodesTpl, data, u)
}

//go:embed assets/portal-my-nodes.html
var portalMyNodesHTML string

var portalMyNodesTpl = template.Must(template.New("my-nodes").Parse(portalMyNodesHTML))
