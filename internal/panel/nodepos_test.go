package panel

import "testing"

func TestNodePosition(t *testing.T) {
	_, _, c := newAdminClient(t)
	cores := []map[string]any{{"type": "xray", "version": "1.8.0"}}
	bad := []map[string]any{
		{"name": "a", "lat": 10.0},
		{"name": "a", "lng": 10.0},
		{"name": "a", "lat": 91.0, "lng": 0.0},
		{"name": "a", "lat": 0.0, "lng": -180.5},
	}
	for _, b := range bad {
		b["cores"] = cores
		if code, _ := c.do("POST", "/nodes", b); code != 400 {
			t.Fatalf("%v accepted: %d", b, code)
		}
	}
	code, out := c.do("POST", "/nodes", map[string]any{"name": "hk", "flag": "hk", "lat": 22.3, "lng": 114.17, "cores": cores})
	if code != 201 {
		t.Fatalf("create %d %v", code, out)
	}
	id := out["node"].(map[string]any)["id"].(string)
	pos := func() (any, any) {
		_, n := c.do("GET", "/nodes/"+id, nil)
		return n["lat"], n["lng"]
	}
	if lat, lng := pos(); lat != 22.3 || lng != 114.17 {
		t.Fatalf("round trip %v %v", lat, lng)
	}
	if code, _ := c.do("PUT", "/nodes/"+id, map[string]any{"name": "hk", "lat": 95.0, "lng": 1.0}); code != 400 {
		t.Fatal("bad update accepted")
	}
	if code, out := c.do("PUT", "/nodes/"+id, map[string]any{"name": "hk", "lat": -33.87, "lng": 151.2}); code != 200 {
		t.Fatalf("update %d %v", code, out)
	}
	if lat, lng := pos(); lat != -33.87 || lng != 151.2 {
		t.Fatalf("after update %v %v", lat, lng)
	}
	// null clears.
	if code, _ := c.do("PUT", "/nodes/"+id, map[string]any{"name": "hk", "lat": nil, "lng": nil}); code != 200 {
		t.Fatal("clear failed")
	}
	if lat, lng := pos(); lat != nil || lng != nil {
		t.Fatalf("not cleared %v %v", lat, lng)
	}
}
