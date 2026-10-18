package plugin

import (
	"strings"
	"testing"
)

var testScopes = []string{"users:read", "users:write"}

func noEvents([]string) error { return nil }

const goodManifest = `{
 "id": "epay", "name": "易支付", "version": "1.0.0", "author": "someone",
 "scopes": ["users:read", "users:write"], "events": ["user.expired"],
 "pages": [{"id": "orders", "title": "订单", "path": "/admin/orders"}],
 "portal": [{"title": "购买", "path": "/shop"}],
 "settings": [{"key": "merchant", "label": "商户号", "type": "string", "required": true},
              {"key": "key", "label": "密钥", "type": "secret"},
              {"key": "mode", "label": "模式", "type": "select", "options": ["live", "test"]}],
 "futureField": 1
}`

func TestParse(t *testing.T) {
	m, err := Parse([]byte(goodManifest), testScopes, noEvents)
	if err != nil || m.ID != "epay" || len(m.Settings) != 3 {
		t.Fatalf("good manifest: %v %+v", err, m)
	}
	bad := map[string]string{
		`{"id":"Bad_ID","name":"x","version":"1.0.0"}`:                                                                   "id",
		`{"id":"ok-id","name":"","version":"1.0.0"}`:                                                                     "name",
		`{"id":"ok-id","name":"x","version":"1.0"}`:                                                                      "version",
		`{"id":"ok-id","name":"x","version":"1.0.0","scopes":["root"]}`:                                                  "权限范围",
		`{"id":"ok-id","name":"x","version":"1.0.0","scopes":["users:read","users:read"]}`:                               "重复",
		`{"id":"ok-id","name":"x","version":"1.0.0","pages":[{"id":"a","title":"t","path":"//evil.com"}]}`:               "path",
		`{"id":"ok-id","name":"x","version":"1.0.0","pages":[{"id":"a","title":"t","path":"/a/../b"}]}`:                  "path",
		`{"id":"ok-id","name":"x","version":"1.0.0","pages":[{"id":"a","title":"t","path":"/a?x=1"}]}`:                   "path",
		`{"id":"ok-id","name":"x","version":"1.0.0","settings":[{"key":"A","label":"l","type":"string"}]}`:               "key",
		`{"id":"ok-id","name":"x","version":"1.0.0","settings":[{"key":"a","label":"l","type":"weird"}]}`:                "type",
		`{"id":"ok-id","name":"x","version":"1.0.0","settings":[{"key":"a","label":"l","type":"select"}]}`:               "options",
		`{"id":"ok-id","name":"x","version":"1.0.0","settings":[{"key":"a","label":"l","type":"secret","default":"x"}]}`: "默认值",
		`not json`: "JSON",
	}
	for in, want := range bad {
		if _, err := Parse([]byte(in), testScopes, noEvents); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want mention of %q", in, err, want)
		}
	}
	if _, err := Parse(make([]byte, MaxManifestBytes+1), testScopes, noEvents); err == nil {
		t.Error("oversized manifest accepted")
	}
}

func TestCheckValues(t *testing.T) {
	m, _ := Parse([]byte(goodManifest), testScopes, noEvents)
	if err := m.CheckValues(map[string]string{"merchant": "1001", "key": "s3cret", "mode": "live"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckValues(map[string]string{"key": "x"}, nil); err == nil {
		t.Error("missing required field accepted")
	}
	if err := m.CheckValues(map[string]string{"merchant": "1", "mode": "other"}, nil); err == nil {
		t.Error("select outside options accepted")
	}
	if err := m.CheckValues(map[string]string{"merchant": "1", "nope": "x"}, nil); err == nil {
		t.Error("unknown key accepted")
	}
	// A stored secret may stay unchanged.
	if err := m.CheckValues(map[string]string{"merchant": "1"}, map[string]bool{"key": true}); err != nil {
		t.Fatal(err)
	}
}

func TestAtLeast(t *testing.T) {
	for _, x := range []struct {
		have, want string
		ok         bool
	}{
		{"v0.1.40", "0.1.34", true}, {"0.1.34", "0.1.40", false}, {"v1.0.0", "0.9.9", true},
		{"dev", "9.9.9", true}, {"0.1.40-rc1", "0.1.40", true}, {"0.2.0", "0.10.0", false},
	} {
		if AtLeast(x.have, x.want) != x.ok {
			t.Errorf("AtLeast(%q,%q) != %v", x.have, x.want, x.ok)
		}
	}
}
