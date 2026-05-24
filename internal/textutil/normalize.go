package textutil

import (
	"strings"
	"unicode/utf8"
)

func NormalizePrompt(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.Trim(line, " \t")
	}

	out := strings.Join(lines, "\n")
	for strings.Contains(out, "\n\n\n") {
		out = strings.ReplaceAll(out, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(out)
}

func Limit(text string, maxBytes, maxLines int) (string, bool) {
	if maxBytes <= 0 && maxLines <= 0 {
		return text, false
	}

	truncated := false
	if maxLines > 0 {
		lines := strings.SplitAfter(text, "\n")
		if len(lines) > maxLines {
			text = strings.Join(lines[:maxLines], "")
			truncated = true
		}
	}

	if maxBytes > 0 && len(text) > maxBytes {
		text = text[:maxBytes]
		for !utf8.ValidString(text) && len(text) > 0 {
			text = text[:len(text)-1]
		}
		truncated = true
	}

	if truncated {
		text = strings.TrimRight(text, "\n") + "\n[truncated]\n"
	}
	return text, truncated
}

func WrapBody(text string, width int) string {
	if width <= 0 {
		return text
	}

	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			out = append(out, "")
			continue
		}
		for len(trimmed) > width {
			cut := strings.LastIndex(trimmed[:width+1], " ")
			if cut <= 0 {
				cut = width
			}
			out = append(out, strings.TrimSpace(trimmed[:cut]))
			trimmed = strings.TrimSpace(trimmed[cut:])
		}
		out = append(out, trimmed)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
