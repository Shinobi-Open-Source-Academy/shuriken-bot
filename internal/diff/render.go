package diff

import (
	"fmt"
	"strings"
)

// Render formats the included files as a single text block for the prompt.
//
// The output is wrapped in an explicit data delimiter. Everything inside it is
// untrusted: it is written by whoever opened the pull request.
func Render(files []File) string {
	var b strings.Builder
	for _, f := range files {
		b.WriteString("=== file: ")
		b.WriteString(f.Path)
		if f.PrevPath != "" && f.PrevPath != f.Path {
			b.WriteString(" (renamed from ")
			b.WriteString(f.PrevPath)
			b.WriteString(")")
		}
		b.WriteString(fmt.Sprintf(" [%s, +%d -%d]\n", f.Status, f.Additions, f.Deletions))
		b.WriteString(f.Patch)
		if !strings.HasSuffix(f.Patch, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	return b.String()
}
