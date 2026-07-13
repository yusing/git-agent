package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bytedance/sonic"
)

const IndexRemoteKey = "index.remote"

type File struct {
	Index IndexConfig `json:"index,omitzero"`
}

type IndexConfig struct {
	Remote string `json:"remote,omitempty"`
}

func LoadFile() (File, error) {
	path, err := filePath()
	if err != nil {
		return File{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, fmt.Errorf("read config: %w", err)
	}
	var cfg File
	if err := sonic.Unmarshal(data, &cfg); err != nil {
		return File{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}

func SaveFile(cfg File) error {
	path, err := filePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := sonic.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create config temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish config: %w", err)
	}
	return nil
}

func filePath() (string, error) {
	root := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".config")
	}
	return filepath.Join(root, "git-agent", "config.json"), nil
}
