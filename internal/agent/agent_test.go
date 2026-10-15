package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
)

func spec(users ...model.User) *model.NodeSpec {
	return &model.NodeSpec{Inbounds: []model.Inbound{{Tag: "a", Core: model.CoreXray, Port: 1, Enabled: true,
		Protocol: model.ProtocolSpec{Type: model.ProtoVLESS}, Users: users}}}
}

func TestUserOnlyDiff(t *testing.T) {
	u1 := model.User{ID: "u1", UUID: "1"}
	u2 := model.User{ID: "u2", UUID: "2"}
	adds, removes, ok := userOnlyDiff(spec(u1), spec(u2), model.CoreXray)
	if !ok || len(adds["a"]) != 1 || adds["a"][0].ID != "u2" || len(removes["a"]) != 1 || removes["a"][0] != "u1" {
		t.Fatalf("adds=%v removes=%v ok=%v", adds, removes, ok)
	}
	// Changed credentials: remove + re-add.
	u1b := u1
	u1b.UUID = "x"
	adds, removes, ok = userOnlyDiff(spec(u1), spec(u1b), model.CoreXray)
	if !ok || len(adds["a"]) != 1 || len(removes["a"]) != 1 {
		t.Fatalf("credential change: %v %v", adds, removes)
	}
	// Structural change is not users-only.
	s2 := spec(u1)
	s2.Inbounds[0].Port = 2
	if _, _, ok := userOnlyDiff(spec(u1), s2, model.CoreXray); ok {
		t.Fatal("port change must not be users-only")
	}
	// Changes on another core do not matter for this one.
	s3 := spec(u1)
	s3.Inbounds = append(s3.Inbounds, model.Inbound{Tag: "b", Core: model.CoreSingbox, Enabled: true})
	if _, _, ok := userOnlyDiff(spec(u1), s3, model.CoreXray); !ok {
		t.Fatal("other core's inbound must not affect xray diff")
	}
}

func TestReleaseURLs(t *testing.T) {
	r, err := releaseFor(model.CoreSingbox, "1.13.5")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := releaseByURL(r.url)
	if err != nil || rt.repo != "SagerNet/sing-box" || rt.tag != "v1.13.5" {
		t.Fatalf("%+v %v", rt, err)
	}
	h, _ := releaseFor(model.CoreHysteria, "2.6.1")
	if rt, _ := releaseByURL(h.url); rt.tag != "app/v2.6.1" {
		t.Fatalf("hysteria tag %q", rt.tag)
	}
	i := &Installer{Mirror: "https://mirror.example/gh"}
	if got := i.mirror(r.url); got != "https://mirror.example/gh/SagerNet/sing-box/releases/download/v1.13.5/"+filepath.Base(r.url) {
		t.Fatalf("mirror: %s", got)
	}
}

func TestExtractTarGz(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"sing-box-1.0.0-linux-amd64/sing-box": "#!bin", "sing-box-1.0.0-linux-amd64/LICENSE": "gpl"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	dst := t.TempDir()
	if err := extract("x.tar.gz", buf.Bytes(), dst, "sing-box-1.0.0-linux-amd64/sing-box", "sing-box"); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dst, "sing-box"))
	if err != nil || st.Mode()&0o111 == 0 {
		t.Fatalf("binary not executable: %v", err)
	}
}
