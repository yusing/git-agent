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
	if host, scpPath, ok := splitSCP(value); ok {
		return networkIdentity(host, scpPath)
	}
	parsed, err := url.Parse(value)
	if err == nil && parsed.Scheme != "" {
		if parsed.Scheme == "file" {
			filePath := parsed.Path
			if !filepath.IsAbs(filePath) {
				if absolute, absErr := filepath.Abs(filePath); absErr == nil {
					filePath = absolute
				}
			}
			return "file:" + filepath.Clean(filePath)
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

// Stable reports whether value has a working-directory-independent identity.
func Stable(value string) bool {
	value = strings.TrimSpace(value)
	if filepath.IsAbs(value) {
		return true
	}
	if _, _, ok := splitSCP(value); ok {
		return true
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return false
	}
	if parsed.Scheme == "file" {
		return parsed.Host == "" && filepath.IsAbs(parsed.Path)
	}
	return parsed.Host != ""
}

func splitSCP(value string) (host, repoPath string, ok bool) {
	host, repoPath, ok = strings.Cut(value, ":")
	if !ok || host == "" || repoPath == "" || strings.ContainsAny(host, "/\\") || strings.HasPrefix(repoPath, "/") || strings.HasPrefix(repoPath, "\\") {
		return "", "", false
	}
	if !strings.Contains(host, "@") {
		switch strings.ToLower(host) {
		case "file", "git", "git+ssh", "http", "https", "ssh":
			return "", "", false
		}
		if len(host) == 1 {
			return "", "", false
		}
	}
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	return host, repoPath, host != ""
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
