package panel

import (
	"fmt"

	"github.com/cuijianzhuang/polaris/internal/store"
)

// Do not fan raw hop addresses out to node operators or external webhooks.
// They remain in the administrator-only historical detail endpoint.
func routeChangeEvent(node store.Node, row store.NodeRouteTrace) Event {
	c := row.Comparison.V
	return Event{
		Kind: "route_changed", NodeID: node.ID,
		ResourceID: notificationHash(row.Family, row.Target),
		Text:       fmt.Sprintf("🧭 节点 %s 观察到检测路径变化\n目标：%s（%s）\n变化跳点：%v\n历史记录：%d → %d\n可在回程检测历史中查看详情。", node.Name, row.Target, row.Family, c.ChangedTTLs, c.PreviousID, row.ID),
		Data:       map[string]any{"node_id": node.ID, "family": row.Family, "target": row.Target, "trace_id": row.ID, "previous_trace_id": c.PreviousID, "changed_ttls": append([]int{}, c.ChangedTTLs...), "observation_only": true},
	}
}
