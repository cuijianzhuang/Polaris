package panel

import (
	"strings"
	"testing"

	"github.com/cuijianzhuang/polaris/internal/store"
)

func TestSubTemplateRevisionProtectsConcurrentEdits(t *testing.T) {
	s, _, client := newAdminClient(t)
	row := store.SubTemplate{ID: "revision-template", Format: "clash", Name: "revision fixture", Content: defaultTemplate("clash")}
	if err := s.st.DB.Create(&row).Error; err != nil {
		t.Fatal(err)
	}
	original := row.Revision
	first := row.Content + "\n# first saved draft\n"
	code, result := client.do("PUT", "/sub-templates/"+row.ID, map[string]any{"content": first, "revision": original})
	if code != 200 || result["revision"] != float64(original+1) {
		t.Fatal(code, result)
	}
	code, result = client.do("PUT", "/sub-templates/"+row.ID, map[string]any{"content": row.Content + "\n# stale draft\n", "revision": original})
	if code != 409 || result["code"] != "template_conflict" {
		t.Fatal("stale edit accepted", code, result)
	}
	var saved store.SubTemplate
	s.st.DB.First(&saved, "id = ?", row.ID)
	if saved.Content != first {
		t.Fatal("newer content overwritten")
	}
	if code, _ := client.do("DELETE", "/sub-templates/"+row.ID, map[string]any{"revision": original}); code != 409 {
		t.Fatal("stale delete accepted", code)
	}
	code, result = client.do("PUT", "/sub-templates/"+row.ID, map[string]any{"content": "{{broken", "isDefault": true, "revision": saved.Revision})
	if code != 400 {
		t.Fatal("invalid template accepted", code, result)
	}
	s.st.DB.First(&saved, "id = ?", row.ID)
	if saved.IsDefault || saved.Content != first || saved.Revision != original+1 {
		t.Fatal("failed validation changed persistent state", saved)
	}
	// Compatibility writes still advance the revision seen by newer editors.
	if code, _ := client.do("PUT", "/sub-templates/"+row.ID, map[string]any{"name": "legacy rename"}); code != 200 {
		t.Fatal(code)
	}
	if code, result := client.do("PUT", "/sub-templates/"+row.ID, map[string]any{"content": first, "revision": saved.Revision}); code != 409 {
		t.Fatal("legacy write did not invalidate an existing draft", code, result)
	}
	s.st.DB.First(&saved, "id = ?", row.ID)
	if saved.Revision != original+2 || !strings.Contains(saved.Name, "legacy") {
		t.Fatal(saved)
	}
}
