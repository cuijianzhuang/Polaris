package panel

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"sync"

	projectdocs "github.com/cuijianzhuang/polaris/docs"
)

type aiKnowledgeSource struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Document string `json:"document"`
	Excerpt  string `json:"excerpt"`
}
type aiKnowledgeChunk struct {
	Title, Document, Text string
	Portal                bool
}

func aiSnippet(text, query string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	start := 0
	for _, term := range aiTerms(query) {
		if index := strings.Index(strings.ToLower(text), term); index >= 0 {
			start = max(0, len([]rune(text[:index]))-120)
			break
		}
	}
	end := min(len(r), start+n)
	return string(r[start:end])
}

var aiKnowledgeIndex struct {
	sync.Once
	Chunks []aiKnowledgeChunk
}
var aiWords = regexp.MustCompile(`[A-Za-z0-9_-]{2,}|[\p{Han}]+`)

func aiTerms(query string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, word := range aiWords.FindAllString(strings.ToLower(query), -1) {
		r := []rune(word)
		parts := []string{word}
		if len(r) > 1 && r[0] > 127 {
			parts = nil
			for i := 0; i+1 < len(r); i++ {
				parts = append(parts, string(r[i:i+2]))
			}
		}
		for _, p := range parts {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}
func loadAIKnowledge() {
	aiKnowledgeIndex.Do(func() {
		fs.WalkDir(projectdocs.Knowledge, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			data, err := projectdocs.Knowledge.ReadFile(path)
			if err != nil {
				return nil
			}
			title := path
			portal := false
			var text []string
			flush := func() {
				body := strings.TrimSpace(strings.Join(text, "\n"))
				r := []rune(body)
				for len(r) > 0 {
					n := min(len(r), 1200)
					aiKnowledgeIndex.Chunks = append(aiKnowledgeIndex.Chunks, aiKnowledgeChunk{title, "docs/" + path, string(r[:n]), portal})
					r = r[n:]
				}
				text = nil
			}
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "#") {
					flush()
					title = strings.TrimSpace(strings.TrimLeft(line, "#"))
					if strings.HasPrefix(line, "## ") {
						portal = path == "user-guide.md" && (strings.HasPrefix(title, "4.") || strings.HasPrefix(title, "5."))
					}
				}
				text = append(text, line)
			}
			flush()
			return nil
		})
	})
}
func aiKnowledgeFor(query, custom string, portal bool) []aiKnowledgeSource {
	loadAIKnowledge()
	type scored struct {
		C     aiKnowledgeChunk
		Score int
	}
	matches := []scored{}
	terms := aiTerms(query)
	for _, chunk := range aiKnowledgeIndex.Chunks {
		if portal && !chunk.Portal {
			continue
		}
		title, body := strings.ToLower(chunk.Title), strings.ToLower(chunk.Text)
		score := 0
		for _, term := range terms {
			if strings.Contains(title, term) {
				score += 6
			}
			score += min(strings.Count(body, term), 3)
		}
		if score > 0 {
			matches = append(matches, scored{chunk, score})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Score > matches[j].Score })
	out := []aiKnowledgeSource{}
	for _, m := range matches {
		if len(out) == 4 {
			break
		}
		out = append(out, aiKnowledgeSource{Title: m.C.Title, Document: m.C.Document, Excerpt: aiRedact(aiSnippet(m.C.Text, query, 700))})
	}
	if strings.TrimSpace(custom) != "" {
		out = append(out, aiKnowledgeSource{Title: "站点 FAQ", Document: "管理员维护的用户知识库", Excerpt: aiRedact(aiSnippet(custom, query, 900))})
	}
	for i := range out {
		out[i].ID = string(rune('1' + i))
		out[i].ID = "K" + out[i].ID
	}
	return out
}
