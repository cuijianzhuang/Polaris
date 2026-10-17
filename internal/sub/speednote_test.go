package sub

import (
	"strings"
	"testing"
)

func TestPageShowsSpeedNote(t *testing.T) {
	b, err := RenderPage(Info{Title: "T", Username: "alice", URL: "https://x/sub/abc", SpeedNote: "↑ 不限 · ↓ 50 Mbps"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "🚀 限速") || !strings.Contains(string(b), "↓ 50 Mbps") {
		t.Fatal("speed note missing from the info page")
	}
	b, _ = RenderPage(Info{Title: "T", URL: "https://x/sub/abc"})
	if strings.Contains(string(b), "🚀 限速") {
		t.Fatal("speed row shown without a limit")
	}
}
