package sub

import (
	"encoding/json"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func vgNodes() []Node {
	return []Node{
		{Name: "HK direct", Region: "HK", Protocol: model.ProtoVLESS, Server: "hk1.example.com", Port: 443, UUID: "u1", PanelKind: "direct", PanelTag: "hk-direct", TrafficRate: 1},
		{Name: "HK relay", Region: "HK", Protocol: model.ProtoVLESS, Server: "hk1.example.com", Port: 8443, UUID: "u1", PanelKind: "relay", PanelChainID: "chain-a", PanelChainName: "港线A", PanelTag: "hk-relay", TrafficRate: 2},
		{Name: "JP cdn", Region: "JP", Protocol: model.ProtoTrojan, Server: "jp1.example.com", Port: 443, Password: "p", PanelKind: "cdn", PanelTag: "jp-cdn", TrafficRate: 0.5},
		{Name: "US pricey", Region: "US", Protocol: model.ProtoTrojan, Server: "us1.example.com", Port: 443, Password: "p", PanelKind: "direct", PanelTag: "us-direct", TrafficRate: 3},
	}
}

func names(nodes []Node) []string {
	out := []string{}
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

func TestFilterViewGroupDimensions(t *testing.T) {
	nodes := vgNodes()
	cases := []struct {
		name string
		op   Operator
		want []string
	}{
		{"kinds", Operator{Type: "filter", Kinds: []string{"relay"}}, []string{"HK relay"}},
		{"chainIds", Operator{Type: "filter", ChainIDs: []string{"chain-a"}}, []string{"HK relay"}},
		{"tags", Operator{Type: "filter", Tags: []string{"hk-direct", "jp-cdn"}}, []string{"HK direct", "JP cdn"}},
		{"combined", Operator{Type: "filter", Kinds: []string{"direct", "cdn"}, Regions: []string{"JP"}}, []string{"JP cdn"}},
	}
	for _, tc := range cases {
		out, err := ApplyChain(nodes, []Operator{tc.op})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := names(out); len(got) != len(tc.want) {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		} else {
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
				}
			}
		}
	}
	// maxRate: rate 2 and 3 drop out; unset rate counts as 1.
	rate := 1.5
	out, err := ApplyChain(nodes, []Operator{{Type: "filter", MaxRate: &rate}})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(out); len(got) != 2 || got[0] != "HK direct" || got[1] != "JP cdn" {
		t.Fatalf("maxRate: got %v", got)
	}
}

func TestRenameViewGroupVars(t *testing.T) {
	nodes := vgNodes()
	for i := range nodes {
		nodes[i].PanelViewGroup = "亚洲组"
	}
	out, err := ApplyChain(nodes, []Operator{{Type: "rename", Template: "{group} {kind} {chain} {name}"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"亚洲组 直连 HK direct",
		"亚洲组 中转 港线A HK relay",
		"亚洲组 CDN JP cdn",
		"亚洲组 直连 US pricey",
	}
	if got := names(out); len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	} else {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	}
}

func TestSortByRate(t *testing.T) {
	out, err := ApplyChain(vgNodes(), []Operator{{Type: "sort", SortBy: []string{"rate"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"JP cdn", "HK direct", "HK relay", "US pricey"}
	if got := names(out); len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	} else {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v want %v", got, want)
			}
		}
	}
	desc, err := ApplyChain(vgNodes(), []Operator{{Type: "sort", SortBy: []string{"rate"}, Desc: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(desc); got[0] != "US pricey" || got[len(got)-1] != "JP cdn" {
		t.Fatalf("desc: got %v", got)
	}
}

func TestSplitByFilterPredicate(t *testing.T) {
	matched, rest, err := SplitByFilter(vgNodes(), Operator{Type: "filter", Regions: []string{"HK"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(matched); len(got) != 2 || got[0] != "HK direct" || got[1] != "HK relay" {
		t.Fatalf("matched %v", got)
	}
	if got := names(rest); len(got) != 2 || got[0] != "JP cdn" || got[1] != "US pricey" {
		t.Fatalf("rest %v", got)
	}
}

// Legacy operator JSON (written before the view-group fields existed) must
// keep parsing with the new fields unset, and the new fields must round-trip.
func TestOperatorJSONCompat(t *testing.T) {
	var op Operator
	if err := json.Unmarshal([]byte(`{"type":"filter","protocols":["vless"],"include":"HK"}`), &op); err != nil {
		t.Fatal(err)
	}
	if op.Kinds != nil || op.ChainIDs != nil || op.MaxRate != nil || op.Tags != nil {
		t.Fatalf("legacy JSON gained fields: %+v", op)
	}
	rate := 2.0
	op = Operator{Type: "filter", Kinds: []string{"relay"}, ChainIDs: []string{"c1"}, MaxRate: &rate, Tags: []string{"t1"}}
	b, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	var back Operator
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Kinds) != 1 || back.Kinds[0] != "relay" || back.MaxRate == nil || *back.MaxRate != 2 || back.Tags[0] != "t1" {
		t.Fatalf("round trip lost fields: %s", b)
	}
}

func TestValidateChainViewGroupFields(t *testing.T) {
	if err := ValidateChain([]Operator{{Type: "filter", Kinds: []string{"bogus"}}}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	neg := -1.0
	if err := ValidateChain([]Operator{{Type: "filter", MaxRate: &neg}}); err == nil {
		t.Fatal("negative maxRate accepted")
	}
	// The compiled-regex cache returns the same instance for a pattern.
	a, err := compileRe("HK|JP")
	if err != nil {
		t.Fatal(err)
	}
	b, err := compileRe("HK|JP")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("regex cache miss: patterns recompiled per call")
	}
}
