package flags

import (
	"strings"
	"testing"
)

func TestTextPreservesEscapingAroundFlag(t *testing.T) {
	input := `🇹🇼 <img src=x onerror="alert(1)"> & 'quoted' 🇹🇼`
	out := string(Text(input))
	if strings.Count(out, `<svg `) != 2 || strings.Contains(out, `<img`) || !strings.Contains(out, `&lt;img`) || !strings.Contains(out, `&amp;`) {
		t.Fatalf("unsafe or missing flag replacement: %s", out)
	}
	if got := string(Text(`JP & <Taiwan>`)); got != `JP &amp; &lt;Taiwan&gt;` {
		t.Fatalf("ordinary text changed: %s", got)
	}
}
