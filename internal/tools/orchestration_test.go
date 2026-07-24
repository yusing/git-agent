package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yusing/git-agent/internal/gitctx"
)

func TestOrchestrationManifestConfinesAndPinsArtifacts(t *testing.T) {
	dir := t.TempDir()
	artifactPath := filepath.Join(dir, "report.md")
	content := []byte("one\ntwo\n")
	if err := os.WriteFile(artifactPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	manifestPath := filepath.Join(dir, "manifest.json")
	manifestJSON := fmt.Sprintf(`{"artifacts":[{"id":"review","path":%q,"size":%d,"sha256":%q}]}`, artifactPath, len(content), hex.EncodeToString(sum[:]))
	if err := os.WriteFile(manifestPath, []byte(manifestJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadOrchestrationManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(manifest.Inventory(), artifactPath) {
		t.Fatalf("inventory leaked artifact path: %s", manifest.Inventory())
	}
	registry := NewReviewRegistry(nil, nil, ReviewModeCodebase, ReviewScope{}, gitctx.ChangeFingerprint{}, manifest)
	result, err := registry.Execute(t.Context(), Invocation{Name: OrchestrationArtifactToolName, Arguments: `{"id":"review","line_start":2}`})
	if err != nil || !strings.Contains(result.Content, `"content": "two\n"`) {
		t.Fatalf("result = %s, error = %v", result.Content, err)
	}
	if err := os.WriteFile(artifactPath, []byte("changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Execute(t.Context(), Invocation{Name: OrchestrationArtifactToolName, Arguments: `{"id":"review"}`}); err == nil || !strings.Contains(err.Error(), "changed after validation") {
		t.Fatalf("mutation error = %v", err)
	}
}

func TestOrchestrationManifestRejectsEscape(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("x"))
	path := filepath.Join(dir, "manifest.json")
	data := fmt.Sprintf(`{"artifacts":[{"id":"outside","path":%q,"size":1,"sha256":%q}]}`, outside, hex.EncodeToString(sum[:]))
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrchestrationManifest(path); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escape error = %v", err)
	}
}

func TestOrchestrationManifestRejectsIntermediateSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("x"))
	path := filepath.Join(dir, "manifest.json")
	data := fmt.Sprintf(`{"artifacts":[{"id":"outside","path":%q,"size":1,"sha256":%q}]}`, filepath.Join(dir, "link", "outside"), hex.EncodeToString(sum[:]))
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrchestrationManifest(path); err == nil {
		t.Fatal("manifest followed intermediate symlink outside root")
	}
}
