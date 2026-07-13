// Package giturl normalizes and sanitizes Git remote URLs.
package giturl

import (
	"net"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// Sanitize removes credentials and non-address URL components.
func Sanitize(value string) string {
	trimmed := strings.TrimSpace(value)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" {
		return trimmed
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return parsed.String()
}

// Identity returns one stable repository identity for common SSH and HTTP URL
// spellings. Local paths remain path-based and are made absolute when possible.
func Identity(value string) string {
	value = strings.TrimSpace(value)
	if scpHost, scpPath, ok := strings.Cut(value, ":"); ok && !strings.Contains(scpHost, "/") && strings.Contains(scpHost, "@") {
		host := scpHost[strings.LastIndexByte(scpHost, '@')+1:]
		return networkIdentity(host, scpPath)
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.Scheme != "" {
		if parsed.Scheme == "file" {
			return "file:" + filepath.Clean(parsed.Path)
		}
		if parsed.Host != "" {
			host := parsed.Hostname()
			if port := parsed.Port(); port != "" && port != defaultPort(parsed.Scheme) {
				host = net.JoinHostPort(host, port)
			} else if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			return networkIdentity(host, parsed.Path)
		}
	}
	if absolute, err := filepath.Abs(value); err == nil {
		return "file:" + filepath.Clean(absolute)
	}
	return "file:" + filepath.Clean(value)
}

func defaultPort(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	case "ssh", "git+ssh":
		return "22"
	default:
		return ""
	}
}

func networkIdentity(host, repoPath string) string {
	repoPath = strings.TrimSuffix(strings.Trim(path.Clean("/"+repoPath), "/"), ".git")
	return strings.ToLower(host) + "/" + repoPath
}
