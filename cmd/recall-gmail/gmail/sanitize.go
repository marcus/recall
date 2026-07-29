package gmail

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	spacesPattern = regexp.MustCompile(`[  ]+`)
	urlPattern    = regexp.MustCompile(
		`(?i)(^|[^@[:alnum:]_.-])((?:https?://|www\.)\S+|` +
			`(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}[/?#]\S*)`,
	)
)

func sanitizeLine(text string) string {
	var out strings.Builder
	for _, r := range text {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029':
			out.WriteByte(' ')
		case unsafeControl(r):
			continue
		default:
			out.WriteRune(r)
		}
	}
	return strings.TrimSpace(spacesPattern.ReplaceAllString(out.String(), " "))
}

func sanitizeBlock(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.ReplaceAll(text, "\u2028", "\n")
	text = strings.ReplaceAll(text, "\u2029", "\n")
	var cleaned strings.Builder
	for _, r := range text {
		if !unsafeControl(r) || r == '\n' || r == '\t' {
			cleaned.WriteRune(r)
		}
	}

	lines := strings.Split(cleaned.String(), "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, line := range lines {
		line = strings.TrimRight(spacesPattern.ReplaceAllString(line, " "), " ")
		if line == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func unsafeControl(r rune) bool {
	return (r < 0x20 && r != '\t' && r != '\n' && r != '\r') ||
		(r >= 0x7f && r <= 0x9f) ||
		(r >= 0x202a && r <= 0x202e) ||
		(r >= 0x2066 && r <= 0x2069)
}

func stripURLs(text string) string {
	return urlPattern.ReplaceAllString(text, "${1}[url removed]")
}

func containsURL(text string) bool {
	return urlPattern.MatchString(text)
}

func clipBytes(text string, limit int) (string, bool) {
	if limit <= 0 || len(text) <= limit {
		return text, false
	}
	const marker = "…"
	cut := limit
	if limit >= len(marker) {
		cut -= len(marker)
	}
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	clipped := strings.TrimSpace(text[:cut])
	if len(clipped)+len(marker) <= limit {
		clipped += marker
	}
	return clipped, true
}

func clipPreview(text string) string {
	got, cut := clipBytes(text, excerptBytes)
	if !cut {
		return got
	}
	return got
}

func tokenize(text string) []string {
	var (
		out   []string
		token []rune
	)
	flush := func() {
		got := strings.Trim(string(token), "-_.@")
		if got != "" {
			out = append(out, got)
		}
		token = token[:0]
	}
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-_.@", r) {
			token = append(token, r)
		} else if len(token) > 0 {
			flush()
		}
	}
	if len(token) > 0 {
		flush()
	}
	return out
}
