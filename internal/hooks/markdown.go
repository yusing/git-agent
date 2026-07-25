package hooks

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

func formatMarkdown(payload PostInspection) string {
	var text strings.Builder
	writeMarkdownField(&text, "Session ID", payload.Session.ID)
	writeMarkdownField(&text, "Command", payload.Session.Command)
	writeMarkdownField(&text, "Mode", payload.Session.Mode)
	writeMarkdownField(&text, "Model", payload.Session.Model)
	writeMarkdownField(&text, "Reasoning effort", payload.Session.ReasoningEffort)
	writeMarkdownField(&text, "Elapsed", (time.Duration(payload.Session.ElapsedMS) * time.Millisecond).String())

	text.WriteString("\n## Usage\n\n")
	writeMarkdownUsage(&text, payload.Metrics.Usage)

	fmt.Fprintf(&text, "\n## Branches (%d)\n", payload.Metrics.BranchesCreated)
	if len(payload.Metrics.Branches) == 0 {
		text.WriteString("\nNone\n")
	}
	for _, branch := range payload.Metrics.Branches {
		fmt.Fprintf(&text, "\n### Branch %s\n\n", markdownCode(branch.ID))
		writeMarkdownField(&text, "Parent", branch.ParentID)
		writeMarkdownField(&text, "Model", branch.Model)
		writeMarkdownField(&text, "Reasoning effort", branch.ReasoningEffort)
		writeMarkdownUsage(&text, branch.Usage)
	}

	report, ok := markdownObject(payload.Report)
	if !ok {
		text.WriteString("\n## Report\n\nUnavailable\n")
		return text.String()
	}
	if summary := markdownString(report["summary"]); summary != "" {
		text.WriteString("\n## Summary\n\n")
		text.WriteString(escapeMarkdown(summary))
		text.WriteByte('\n')
	}
	if recommendation := markdownString(report["recommendation"]); recommendation != "" {
		text.WriteString("\n")
		writeMarkdownField(&text, "Recommendation", recommendation)
	}

	if findings, exists := report["findings"]; exists {
		writeMarkdownFindings(&text, findings)
	} else if opportunities, exists := report["opportunities"]; exists {
		writeMarkdownOpportunities(&text, opportunities)
	} else {
		text.WriteString("\n## Report items\n\nNo findings or opportunities field.\n")
	}
	writeMarkdownChecks(&text, report["checks"])
	return text.String()
}

func writeMarkdownUsage(text *strings.Builder, usage Usage) {
	fmt.Fprintf(text, "- **Input tokens:** %d\n", usage.InputTokens)
	fmt.Fprintf(text, "  - Cached: %d\n", usage.CachedInputTokens)
	fmt.Fprintf(text, "  - Uncached: %d\n", usage.UncachedInputTokens)
	fmt.Fprintf(text, "- **Output tokens:** %d\n", usage.OutputTokens)
	fmt.Fprintf(text, "  - Reasoning: %d\n", usage.ReasoningTokens)
	fmt.Fprintf(text, "- **Total tokens:** %d\n", usage.TotalTokens)
}

func writeMarkdownFindings(text *strings.Builder, value any) {
	text.WriteString("\n## Findings\n")
	items, ok := markdownList(value)
	if !ok {
		text.WriteString("\nUnavailable\n")
		return
	}
	if len(items) == 0 {
		text.WriteString("\nNone\n")
		return
	}
	for _, value := range items {
		item, ok := markdownObject(value)
		if !ok {
			text.WriteString("\n### Unavailable finding\n")
			continue
		}
		fmt.Fprintf(text, "\n### %s — %s\n\n", escapeMarkdown(markdownString(item["severity"])), escapeMarkdown(markdownString(item["title"])))
		writeMarkdownField(text, "Aspect", markdownString(item["aspect"]))
		writeMarkdownParagraph(text, "Impact", markdownString(item["impact"]))
		writeMarkdownEvidence(text, item["evidences"])
		writeMarkdownParagraph(text, "Proposed fix", markdownString(item["proposed_fix"]))
	}
}

func writeMarkdownOpportunities(text *strings.Builder, value any) {
	text.WriteString("\n## Opportunities\n")
	items, ok := markdownList(value)
	if !ok {
		text.WriteString("\nUnavailable\n")
		return
	}
	if len(items) == 0 {
		text.WriteString("\nNone\n")
		return
	}
	for _, value := range items {
		item, ok := markdownObject(value)
		if !ok {
			text.WriteString("\n### Unavailable opportunity\n")
			continue
		}
		fmt.Fprintf(text, "\n### %s — %s\n\n", escapeMarkdown(markdownString(item["aspect"])), escapeMarkdown(markdownString(item["title"])))
		writeMarkdownParagraph(text, "Details", markdownString(item["body"]))
		writeMarkdownEvidence(text, item["evidences"])
		writeMarkdownParagraph(text, "Proposed change", markdownString(item["proposed_change"]))
	}
}

func writeMarkdownEvidence(text *strings.Builder, value any) {
	text.WriteString("\n**Evidence**\n\n")
	items, ok := markdownList(value)
	if !ok || len(items) == 0 {
		text.WriteString("- Unavailable\n")
		return
	}
	for _, value := range items {
		item, ok := markdownObject(value)
		if !ok {
			text.WriteString("- Unavailable\n")
			continue
		}
		location := markdownString(item["path"])
		lineStart := markdownString(item["line_start"])
		lineEnd := markdownString(item["line_end"])
		if lineStart != "" {
			location += ":" + lineStart
			if lineEnd != "" && lineEnd != lineStart {
				location += "-" + lineEnd
			}
		}
		fmt.Fprintf(text, "- %s — %s\n", markdownCode(location), escapeMarkdown(markdownString(item["title"])))
	}
}

func writeMarkdownChecks(text *strings.Builder, value any) {
	items, ok := markdownList(value)
	if !ok || len(items) == 0 {
		return
	}
	text.WriteString("\n## Checks\n")
	for _, value := range items {
		check, ok := markdownObject(value)
		if !ok {
			text.WriteString("\n### Unavailable check\n")
			continue
		}
		fmt.Fprintf(text, "\n### %s — %s\n", escapeMarkdown(markdownString(check["name"])), escapeMarkdown(markdownString(check["status"])))
		if reason := markdownString(check["reason"]); reason != "" {
			writeMarkdownParagraph(text, "Reason", reason)
		}
		if checkError := markdownString(check["error"]); checkError != "" {
			writeMarkdownParagraph(text, "Error", checkError)
		}
		diagnostics, ok := markdownList(check["diagnostics"])
		if ok {
			for _, value := range diagnostics {
				diagnostic, ok := markdownObject(value)
				if !ok {
					text.WriteString("\n- Unavailable diagnostic\n")
					continue
				}
				var location strings.Builder
				location.WriteString(markdownString(diagnostic["path"]))
				for _, key := range []string{"line", "column"} {
					if part := markdownString(diagnostic[key]); part != "" {
						location.WriteString(":" + part)
					}
				}
				fmt.Fprintf(text, "\n- %s %s — %s\n", markdownCode(location.String()), markdownCode(markdownString(diagnostic["code"])), escapeMarkdown(markdownString(diagnostic["message"])))
			}
		}
		if omitted := markdownPositiveInt(check["omitted"]); omitted > 0 {
			fmt.Fprintf(text, "\n- **Additional diagnostics omitted:** %d\n", omitted)
		}
	}
}

func writeMarkdownField(text *strings.Builder, label, value string) {
	if value == "" {
		value = "Unavailable"
	}
	fmt.Fprintf(text, "- **%s:** %s\n", label, escapeMarkdown(value))
}

func writeMarkdownParagraph(text *strings.Builder, label, value string) {
	if value == "" {
		value = "Unavailable"
	}
	fmt.Fprintf(text, "\n**%s**\n\n%s\n", label, escapeMarkdown(value))
}

func markdownObject(value any) (map[string]any, bool) {
	if object, ok := value.(map[string]any); ok {
		return object, true
	}
	data, err := sonic.Marshal(value)
	if err != nil {
		return nil, false
	}
	var object map[string]any
	if err := sonic.Unmarshal(data, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func markdownList(value any) ([]any, bool) {
	items, ok := value.([]any)
	return items, ok
}

func markdownString(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func markdownPositiveInt(value any) int64 {
	number, err := strconv.ParseInt(fmt.Sprint(value), 10, 64)
	if err != nil || number < 1 {
		return 0
	}
	return number
}

func markdownCode(value string) string {
	value = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ").Replace(value)
	longest := 0
	current := 0
	for _, char := range value {
		if char == '`' {
			current++
			longest = max(longest, current)
		} else {
			current = 0
		}
	}
	delimiter := strings.Repeat("`", longest+1)
	if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") ||
		strings.HasPrefix(value, " ") || strings.HasSuffix(value, " ") {
		return delimiter + " " + value + " " + delimiter
	}
	return delimiter + value + delimiter
}

func escapeMarkdown(value string) string {
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		lines[index] = markdownEscapeReplacer.Replace(strings.TrimLeft(line, " \t"))
	}
	return strings.Join(lines, "\n")
}

var markdownEscapeReplacer = strings.NewReplacer(
	"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_",
	"[", "\\[", "]", "\\]", "<", "\\<", ">", "\\>",
	"#", "\\#", "|", "\\|", "-", "\\-", "+", "\\+",
	"!", "\\!", "~", "\\~", "=", "\\=", ".", "\\.", ")", "\\)",
)
