package config

import (
	"testing"
	"time"
)

func TestResolveFlagEnvDefaultOrder(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")
	t.Setenv("OPENAI_BASE_URL", "https://env.example/v1")
	t.Setenv("OPENAI_MODEL", "env-model")

	cfg, err := Resolve(Options{
		BaseURL:  "https://flag.example/v1",
		Model:    "flag-model",
		Fast:     true,
		Medium:   true,
		Timeout:  "3s",
		MaxSteps: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "env-key" {
		t.Fatalf("APIKey = %q", cfg.APIKey)
	}
	if cfg.BaseURL != "https://flag.example/v1" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.Model != "flag-model" {
		t.Fatalf("Model = %q", cfg.Model)
	}
	if cfg.ServiceTier != "priority" {
		t.Fatalf("ServiceTier = %q", cfg.ServiceTier)
	}
	if cfg.ThinkingEffort != "medium" {
		t.Fatalf("ThinkingEffort = %q", cfg.ThinkingEffort)
	}
	if cfg.Timeout != 3*time.Second {
		t.Fatalf("Timeout = %s", cfg.Timeout)
	}
	if cfg.MaxSteps != 2 {
		t.Fatalf("MaxSteps = %d", cfg.MaxSteps)
	}
}

func TestResolveRequiresAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	if _, err := Resolve(Options{}); err == nil {
		t.Fatal("expected missing API key error")
	}
}

func TestResolveUsesRaisedDefaultMaxSteps(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")

	cfg, err := Resolve(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxSteps != DefaultMaxSteps {
		t.Fatalf("MaxSteps = %d", cfg.MaxSteps)
	}
	if cfg.MaxSteps != 30 {
		t.Fatalf("default MaxSteps = %d", cfg.MaxSteps)
	}
	if cfg.ServiceTier != "" {
		t.Fatalf("ServiceTier = %q", cfg.ServiceTier)
	}
	if cfg.ThinkingEffort != "" {
		t.Fatalf("ThinkingEffort = %q", cfg.ThinkingEffort)
	}
}

func TestResolveRejectsConflictingThinkingFlags(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")

	if _, err := Resolve(Options{Low: true, Medium: true}); err == nil {
		t.Fatal("expected mutually exclusive thinking flags error")
	}
}
