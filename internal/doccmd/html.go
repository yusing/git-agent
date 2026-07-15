package doccmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const (
	maxHTMLBytes = 4 * 1024 * 1024
	maxTextBytes = 32 * 1024
)

func extractMainContent(path string) (string, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", false, fmt.Errorf("stat Rust documentation: %w", err)
	}
	htmlTruncated := info.Size() > maxHTMLBytes
	file, err := os.Open(path)
	if err != nil {
		return "", false, fmt.Errorf("open Rust documentation: %w", err)
	}
	defer func() { _ = file.Close() }()

	document, err := html.Parse(io.LimitReader(file, maxHTMLBytes))
	if err != nil {
		return "", false, fmt.Errorf("parse Rust documentation HTML: %w", err)
	}
	main := findElementByID(document, "main-content")
	if main == nil {
		return "", false, errors.New("Rust documentation HTML has no #main-content")
	}
	var builder strings.Builder
	appendNodeText(&builder, main)
	text := strings.Join(strings.Fields(builder.String()), " ")
	if len(text) <= maxTextBytes {
		return text, htmlTruncated, nil
	}
	text = text[:maxTextBytes]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text, true, nil
}

func findElementByID(node *html.Node, id string) *html.Node {
	if node.Type == html.ElementNode {
		for _, attribute := range node.Attr {
			if attribute.Key == "id" && attribute.Val == id {
				return node
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findElementByID(child, id); found != nil {
			return found
		}
	}
	return nil
}

func appendNodeText(builder *strings.Builder, node *html.Node) {
	if node.Type == html.ElementNode && (node.Data == "script" || node.Data == "style") {
		return
	}
	if node.Type == html.TextNode {
		builder.WriteString(node.Data)
		builder.WriteByte(' ')
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		appendNodeText(builder, child)
	}
}
