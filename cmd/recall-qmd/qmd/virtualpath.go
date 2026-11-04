package qmd

import (
	"fmt"
	"path"
	"strings"
	"unicode"
)

// qmdVirtualPath matches qmd 2.5.x's handelize at the external CLI seam. Qmd
// returns these virtual handles instead of physical filenames (b.html.md ->
// b-html.md). Keep physical mirror names as specified and resolve handles only
// against the admitted original set; collisions are failures, never guesses.
func qmdVirtualPath(rel string) string {
	parts := strings.Split(strings.ReplaceAll(rel, "___", "/"), "/")
	var out []string
	for i, part := range parts {
		var emoji strings.Builder
		previousSymbol := false
		for _, r := range part {
			switch {
			case unicode.Is(unicode.So, r) || unicode.Is(unicode.Sk, r):
				if previousSymbol {
					emoji.WriteByte('-')
				}
				fmt.Fprintf(&emoji, "%x", r)
				previousSymbol = true
			case unicode.Is(unicode.Mn, r) && previousSymbol:
			default:
				emoji.WriteRune(r)
				previousSymbol = false
			}
		}
		part = emoji.String()
		ext := ""
		if i == len(parts)-1 {
			ext = path.Ext(part)
			valid := len(ext) > 1
			for _, r := range strings.TrimPrefix(ext, ".") {
				valid = valid && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
			}
			if !valid {
				ext = ""
			}
			part = strings.TrimSuffix(part, ext)
		}
		var clean strings.Builder
		separator := false
		for _, r := range part {
			if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '$' {
				if separator && clean.Len() > 0 {
					clean.WriteByte('-')
				}
				clean.WriteRune(r)
				separator = false
			} else {
				separator = true
			}
		}
		name := clean.String() + ext
		if name != "" {
			out = append(out, name)
		}
	}
	return strings.Join(out, "/")
}
