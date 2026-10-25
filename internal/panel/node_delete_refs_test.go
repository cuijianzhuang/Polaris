package panel

import (
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/store"
)

// A node whose inbounds other nodes still point at cannot be deleted: the
// dangling node_ref would leave those nodes unable to render.
func TestDeleteNodeReferenced(t *testing.T) {
	s, _, admin := newAdminClient(t)
	s.st.DB.Create(&store.Node{ID: "na", Name: "A"})
	s.st.DB.Create(&store.Node{ID: "nb", Name: "B"})
	s.st.DB.Create(&store.Inbound{ID: "ib", NodeID: "nb", Tag: "in-b", Port: 4433})
	s.st.DB.Create(&store.Outbound{ID: "oa", NodeID: "na", Tag: "to-b", Type: model.OutNodeRef, NodeRefInboundID: "ib"})
	if st, out := admin.do("DELETE", "/nodes/nb", nil); st != 409 {
		t.Fatalf("referenced node deleted: %d %v", st, out)
	}
	s.st.DB.Delete(&store.Outbound{}, "id = ?", "oa")
	if st, out := admin.do("DELETE", "/nodes/nb", nil); st != 204 {
		t.Fatalf("delete: %d %v", st, out)
	}
}
