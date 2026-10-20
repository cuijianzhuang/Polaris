package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBWH(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "private_key" {
			w.Write([]byte(`{"error":700005,"message":"Authentication failure"}`))
			return
		}
		switch r.URL.Path {
		case "/getServiceInfo":
			w.Write([]byte(`{"error":0,"plan":"20G KVM - PROMO","node_location":"US, California","os":"debian-12","hostname":"hk1",
"plan_ram":1073741824,"plan_disk":21474836480,"plan_monthly_data":1099511627776,"monthly_data_multiplier":1.5,
"data_counter":107374182400,"data_next_reset":1793491200,"ip_addresses":["203.0.113.5"],"suspended":false}`))
		case "/getLiveServiceInfo":
			w.Write([]byte(`{"error":0,"plan_monthly_data":1099511627776,"data_counter":10,"ve_status":"running","ve_used_disk_space_b":5368709120,
"mem_available_kb":524288,"load_average":"0.10 0.20 0.30","is_cpu_throttled":"0"}`))
		}
	}))
	defer srv.Close()
	old := BWHBase
	BWHBase = srv.URL
	defer func() { BWHBase = old }()

	in, err := (&BWH{VEID: "123", APIKey: "private_key"}).ServiceInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if in.DataLimit != 1<<40 || in.DataUsed != 150<<30 || in.NextReset.Unix() != 1793491200 || in.Location != "US, California" || in.IPs[0] != "203.0.113.5" {
		t.Fatalf("info %+v", in)
	}
	live, err := (&BWH{VEID: "123", APIKey: "private_key"}).LiveInfo(context.Background())
	if err != nil || live.Status != "running" || live.DiskUsed != 5<<30 || live.MemAvail != 512<<20 || live.Throttled {
		t.Fatalf("live %+v %v", live, err)
	}
	if _, err := (&BWH{VEID: "123", APIKey: "wrong"}).ServiceInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "Authentication failure") {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := (&BWH{}).ServiceInfo(context.Background()); err == nil {
		t.Fatal("empty credentials accepted")
	}
	// The key never shows in a connection error.
	BWHBase = "http://127.0.0.1:1"
	if _, err := (&BWH{VEID: "1", APIKey: "private_key"}).ServiceInfo(context.Background()); err == nil || strings.Contains(err.Error(), "private_key") {
		t.Fatalf("key leaked: %v", err)
	}
}
