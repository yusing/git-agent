package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bytedance/sonic"
)

const OrchestrationArtifactToolName = "read_orchestration_artifact"

var artifactIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type OrchestrationArtifact struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type OrchestrationManifest struct {
	Artifacts []OrchestrationArtifact `json:"artifacts"`
	Digest    string                  `json:"-"`
	root      string
}

func LoadOrchestrationManifest(path string) (*OrchestrationManifest, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("orchestration artifact manifest path must be canonical and absolute")
	}
	data, err := readOwnerOnlyRegular(path)
	if err != nil {
		return nil, fmt.Errorf("read orchestration artifact manifest: %w", err)
	}
	decoder := sonic.ConfigStd.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest OrchestrationManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode orchestration artifact manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("orchestration artifact manifest contains trailing data")
	}
	if len(manifest.Artifacts) == 0 {
		return nil, errors.New("orchestration artifact manifest is empty")
	}
	root := filepath.Dir(path)
	manifest.root = root
	seen := make(map[string]bool, len(manifest.Artifacts))
	for i := range manifest.Artifacts {
		artifact := &manifest.Artifacts[i]
		if !artifactIDPattern.MatchString(artifact.ID) || seen[artifact.ID] {
			return nil, fmt.Errorf("invalid or duplicate orchestration artifact ID %q", artifact.ID)
		}
		seen[artifact.ID] = true
		if !filepath.IsAbs(artifact.Path) || filepath.Clean(artifact.Path) != artifact.Path || !within(root, artifact.Path) {
			return nil, fmt.Errorf("orchestration artifact %q escapes manifest directory", artifact.ID)
		}
		file, err := manifest.open(*artifact)
		if err != nil {
			return nil, fmt.Errorf("read orchestration artifact %q: %w", artifact.ID, err)
		}
		_ = file.Close()
	}
	manifest.Digest = digest(data)
	sort.Slice(manifest.Artifacts, func(i, j int) bool { return manifest.Artifacts[i].ID < manifest.Artifacts[j].ID })
	return &manifest, nil
}

func (m *OrchestrationManifest) Inventory() string {
	type inventoryArtifact struct {
		ID     string `json:"id"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	artifacts := make([]inventoryArtifact, len(m.Artifacts))
	for i, artifact := range m.Artifacts {
		artifacts[i] = inventoryArtifact{artifact.ID, artifact.Size, artifact.SHA256}
	}
	data, _ := sonic.ConfigStd.Marshal(struct {
		Digest    string              `json:"manifest_sha256"`
		Artifacts []inventoryArtifact `json:"artifacts"`
	}{m.Digest, artifacts})
	return string(data)
}

func (m *OrchestrationManifest) open(artifact OrchestrationArtifact) (*os.File, error) {
	root, err := os.OpenRoot(m.root)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(m.root, artifact.Path)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	file, err := root.Open(rel)
	_ = root.Close()
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() != artifact.Size {
		_ = file.Close()
		return nil, errors.New("file must be owner-only regular file with recorded size")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		_ = file.Close()
		return nil, errors.New("file digest does not match manifest")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func readOwnerOnlyRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("file must be regular and owner-only")
	}
	return os.ReadFile(path)
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type orchestrationArtifactTool struct{ manifest *OrchestrationManifest }

func (t orchestrationArtifactTool) Definition() Definition {
	return Definition{Name: OrchestrationArtifactToolName, Description: "Read one helper-authorized orchestration artifact by ID.", Schema: schema(map[string]any{
		"id":         stringProp("Artifact ID from prepared orchestration inventory."),
		"line_start": intProp("Optional inclusive first line. Zero starts at line 1.", 0, 10000000),
		"line_end":   intProp("Optional inclusive last line. Zero reads through EOF.", 0, 10000000),
		"max_bytes":  intProp("Maximum bytes to return.", 1, 65536),
		"max_lines":  intProp("Maximum lines to return.", 1, 2000),
	}, "id"), Strict: true}
}

func (t orchestrationArtifactTool) Execute(_ context.Context, invocation Invocation) (Result, error) {
	args, err := parseArgs[struct {
		ID        string `json:"id"`
		LineStart int    `json:"line_start"`
		LineEnd   int    `json:"line_end"`
		MaxBytes  int    `json:"max_bytes"`
		MaxLines  int    `json:"max_lines"`
	}](invocation.Arguments)
	if err != nil {
		return Result{}, err
	}
	for _, artifact := range t.manifest.Artifacts {
		if artifact.ID != args.ID {
			continue
		}
		file, err := t.manifest.open(artifact)
		if err != nil {
			return Result{}, fmt.Errorf("orchestration artifact %q changed after validation", artifact.ID)
		}
		defer file.Close()
		maxBytes, maxLines := normalizeCaps(args.MaxBytes, args.MaxLines)
		content, first, last, truncated, err := readLineRange(file, args.LineStart, args.LineEnd, maxBytes, maxLines, false)
		if err != nil {
			return Result{}, err
		}
		return jsonResult(OrchestrationArtifactToolName, map[string]any{"id": artifact.ID, "line_start": first, "line_end": last, "content": content}, truncated)
	}
	return Result{}, fmt.Errorf("unknown orchestration artifact %q", args.ID)
}
