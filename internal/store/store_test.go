package store

import (
	"path/filepath"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/model"
	"github.com/cuijianzhuang/polaris/internal/secret"
)

func TestOpenAndJSON(t *testing.T) {
	box, _ := secret.New("k")
	s, err := Open(filepath.Join(t.TempDir(), "p.db"), box, false)
	if err != nil {
		t.Fatal(err)
	}
	in := Inbound{ID: NewID(), NodeID: "n", Tag: "a", Port: 443,
		Protocol: NewJSON(model.ProtocolSpec{Type: model.ProtoVLESS, Flow: "xtls-rprx-vision"})}
	if err := s.DB.Create(&in).Error; err != nil {
		t.Fatal(err)
	}
	var got Inbound
	if err := s.DB.First(&got, "id = ?", in.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Protocol.V.Flow != "xtls-rprx-vision" {
		t.Fatalf("json column roundtrip failed: %+v", got.Protocol.V)
	}
	dup := Inbound{ID: NewID(), NodeID: "n", Tag: "a", Port: 444}
	if err := s.DB.Create(&dup).Error; !IsDuplicate(err) {
		t.Fatalf("want duplicate error, got %v", err)
	}
}
