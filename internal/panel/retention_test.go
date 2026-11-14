package panel

import (
	"strings"
	"testing"
	"time"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestCleanupPrunesFailedWebhooksAndExpiredClusterOps(t *testing.T) {
	s := newTestServer(t)
	db := s.st.DB
	now := time.Now().UTC()
	db.Create(&store.WebhookDelivery{ID: "old-failed", Status: "failed", CreatedAt: now.AddDate(0, 0, -40)})
	db.Create(&store.WebhookDelivery{ID: "new-failed", Status: "failed", CreatedAt: now.AddDate(0, 0, -3)})
	db.Create(&store.WebhookDelivery{ID: "old-pending", Status: "pending", CreatedAt: now.AddDate(0, 0, -40)})
	db.Create(&store.ClusterOperation{ID: "op-old", Kind: "k", Target: "t", CreatedAt: now.AddDate(0, 0, -10), ExpiresAt: now.AddDate(0, 0, -9)})
	db.Create(&store.ClusterOperation{ID: "op-new", Kind: "k", Target: "t", CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)})
	db.Create(&store.ClusterDelivery{OperationID: "op-old", MemberID: "m1", Status: "pending", NextAt: now})
	db.Create(&store.ClusterDelivery{OperationID: "op-new", MemberID: "m1", Status: "pending", NextAt: now})
	s.cleanup()
	var hooks []string
	db.Model(&store.WebhookDelivery{}).Order("id").Pluck("id", &hooks)
	if strings.Join(hooks, ",") != "new-failed,old-pending" {
		t.Fatalf("webhook deliveries left: %v", hooks)
	}
	var ops, deliveries []string
	db.Model(&store.ClusterOperation{}).Pluck("id", &ops)
	db.Model(&store.ClusterDelivery{}).Pluck("operation_id", &deliveries)
	if strings.Join(ops, ",") != "op-new" || strings.Join(deliveries, ",") != "op-new" {
		t.Fatalf("cluster ops %v deliveries %v", ops, deliveries)
	}
}

func TestValidTagCounter(t *testing.T) {
	for id, want := range map[string]bool{
		"inbound>>>vless-443":                   true,
		"outbound>>>direct":                     true,
		"inbound>>>":                            false,
		"inbound>>>" + strings.Repeat("x", 129): false,
		"inbound>>>" + strings.Repeat("节", 128): true,
		"outbound>>>bad\xff":                    false,
	} {
		if got := validTagCounter(id); got != want {
			t.Errorf("validTagCounter(%q) = %v", id, got)
		}
	}
}
