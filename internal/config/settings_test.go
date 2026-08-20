package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	json "encoding/json/v2"
)

func TestLoadSettingsUsesHomeGitAgentDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".git-agent", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"hooks":{"post_inspection":["notify",""]}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	settings, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got := settings.Hooks.PostInspection; len(got) != 1 || got[0] != "notify" {
		t.Fatalf("post-inspection hooks = %#v", got)
	}
}

func TestLoadSettingsMissingFileIsEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	settings, err := LoadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if len(settings.Hooks.PostInspection) != 0 {
		t.Fatalf("settings = %#v", settings)
	}
}

func TestLoadSettingsRejectsValuesOutsideV1Schema(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".git-agent", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"hooks":{"post_inspection":[]},"future":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadSettings()
	if err == nil || !errors.Is(err, json.ErrUnknownName) {
		t.Fatalf("error = %v", err)
	}
}
