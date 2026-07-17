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
	var paragraph []string
	flush := func() {
		if len(paragraph) == 0 {
			return
		}
		out = append(out, wrapLine(strings.Join(paragraph, " "), width)...)
		paragraph = nil
	}
	for line := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		trimmedRight := strings.TrimRight(line, " \t")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			flush()
			out = append(out, "")
			continue
		}
		if firstPrefix, continuationPrefix, content, ok := wrappableStructuralBodyLine(trimmedRight); ok {
			flush()
			out = append(out, wrapPrefixedLine(firstPrefix, continuationPrefix, content, width)...)
			continue
		}
		if isStructuralBodyLine(line, trimmed) {
			flush()
			out = append(out, trimmedRight)
			continue
		}
		paragraph = append(paragraph, trimmed)
	}
	flush()
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func wrappableStructuralBodyLine(line string) (string, string, string, bool) {
	rest := strings.TrimLeft(line, " \t")
	indent := line[:len(line)-len(rest)]
	quotePrefixLen := 0
	for strings.HasPrefix(rest[quotePrefixLen:], "> ") {
		quotePrefixLen += 2
	}
	quotePrefix := rest[:quotePrefixLen]
	rest = rest[quotePrefixLen:]
	structuralPrefix := indent + quotePrefix

	if len(rest) >= 3 && strings.ContainsRune("-*+", rune(rest[0])) && rest[1] == ' ' {
		return structuralPrefix + rest[:2], structuralPrefix + "  ", rest[2:], true
	}
	if prefixLen := orderedListPrefixLen(rest); prefixLen > 0 {
		prefix := structuralPrefix + rest[:prefixLen]
		return prefix, structuralPrefix + strings.Repeat(" ", prefixLen), rest[prefixLen:], true
	}
	if token, value, ok := strings.Cut(rest, ": "); ok && value != "" && isTrailerToken(token) {
		prefix := structuralPrefix + token + ": "
		return prefix, structuralPrefix + strings.Repeat(" ", len(token)+2), value, true
	}
	if quotePrefix != "" && rest != "" {
		return structuralPrefix, structuralPrefix, rest, true
	}
	return "", "", "", false
}

func wrapPrefixedLine(firstPrefix, continuationPrefix, text string, width int) []string {
	contentWidth := width - len(firstPrefix)
	if contentWidth <= 0 {
		return []string{firstPrefix + text}
	}

	lines := wrapLine(text, contentWidth)
	for i := range lines {
		prefix := continuationPrefix
		if i == 0 {
			prefix = firstPrefix
		}
		lines[i] = prefix + lines[i]
	}
	return lines
}

func isStructuralBodyLine(line, trimmed string) bool {
	if line != strings.TrimLeft(line, " \t") ||
		strings.HasPrefix(trimmed, "- ") ||
		strings.HasPrefix(trimmed, "* ") ||
		strings.HasPrefix(trimmed, "+ ") ||
		strings.HasPrefix(trimmed, "> ") ||
		isOrderedListLine(trimmed) {
		return true
	}
	prefix, _, ok := strings.Cut(trimmed, ": ")
	return ok && isTrailerToken(prefix)
}

func isOrderedListLine(line string) bool {
	return orderedListPrefixLen(line) > 0
}

func orderedListPrefixLen(line string) int {
	for i, r := range line {
		if r < '0' || r > '9' {
			if i > 0 && r == '.' && len(line) > i+1 && line[i+1] == ' ' {
				return i + 2
			}
			return 0
		}
	}
	return 0
}

func isTrailerToken(text string) bool {
	if text == "" {
		return false
	}
	for _, r := range text {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

func wrapLine(text string, width int) []string {
	var out []string
	for len(text) > width {
		cut := strings.LastIndex(text[:width+1], " ")
		if cut <= 0 {
			nextSpace := strings.Index(text, " ")
			if nextSpace < 0 {
				return append(out, text)
			}
			out = append(out, strings.TrimSpace(text[:nextSpace]))
			text = strings.TrimSpace(text[nextSpace:])
			continue
		}
		out = append(out, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}
	return append(out, text)
}
