package panel

import (
	"slices"
	"strings"
)

func (f *fakeTG) markups() [][][]button {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][][]button
	for _, m := range f.sent {
		var rows [][]button
		if rm, ok := m["reply_markup"].(map[string]any); ok {
			kb, _ := rm["inline_keyboard"].([]any)
			for _, r := range kb {
				var row []button
				for _, b := range r.([]any) {
					bm := b.(map[string]any)
					d, _ := bm["callback_data"].(string)
					u, _ := bm["url"].(string)
					row = append(row, button{Text: bm["text"].(string), Data: d, URL: u})
				}
				rows = append(rows, row)
			}
		}
		out = append(out, rows)
	}
	return out
}

func eachButton(f *fakeTG, fn func(button) bool) bool {
	for _, kb := range f.markups() {
		for _, r := range kb {
			if slices.ContainsFunc(r, fn) {
				return true
			}
		}
	}
	return false
}

func hasButton(f *fakeTG, data string) bool {
	return eachButton(f, func(b button) bool { return b.Data == data })
}

func hasButtonText(f *fakeTG, text string) bool {
	return eachButton(f, func(b button) bool { return strings.Contains(b.Text, text) })
}

func buttonURLs(f *fakeTG) []string {
	var out []string
	eachButton(f, func(b button) bool {
		if b.URL != "" {
			out = append(out, b.URL)
		}
		return false
	})
	return out
}
