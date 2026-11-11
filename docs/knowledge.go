// Package docs supplies the versioned project manuals to the optional AI
// retriever. No network fetch or external vector database is required.
package docs

import "embed"

//go:embed user-guide.md operations/*.md
var Knowledge embed.FS
