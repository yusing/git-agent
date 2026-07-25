package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/metadata"
)

// Settings is the v1 global settings schema.
type Settings struct {
	Hooks HookSettings `json:"hooks,omitzero"`
}

type HookSettings struct {
	PostInspection []string `json:"post_inspection,omitempty"`
}

func LoadSettings() (Settings, error) {
	path, err := settingsPath()
	if err != nil {
		return Settings{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, fmt.Errorf("read settings: %w", err)
	}
	decoder := sonic.ConfigStd.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var settings Settings
	if err := decoder.Decode(&settings); err != nil {
		return Settings{}, fmt.Errorf("parse settings %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Settings{}, fmt.Errorf("parse settings %s: %w", path, err)
	}
	settings.Hooks.PostInspection = slices.DeleteFunc(settings.Hooks.PostInspection, func(source string) bool {
		return strings.TrimSpace(source) == ""
	})
	return settings, nil
}

func settingsPath() (string, error) {
	root, err := metadata.Root()
	if err != nil {
		return "", fmt.Errorf("resolve metadata root for settings: %w", err)
	}
	return filepath.Join(root, "settings.json"), nil
}
