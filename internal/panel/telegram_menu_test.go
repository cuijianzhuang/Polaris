package panel

import (
	"strings"
)

func buttonData(rows [][]button) string {
	var b strings.Builder
	for _, r := range rows {
		for _, x := range r {
			b.WriteString(x.Data + " " + x.Text + "\n")
		}
	}
	return b.String()
}
