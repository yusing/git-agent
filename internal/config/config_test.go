package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveFlagEnvDefaultOrder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "env-key")
	t.Setenv("OPENAI_BASE_URL", "https://env.example/v1")
	t.Setenv("OPENAI_MODEL", "env-model")

	cfg, err := Resolve(Options{
		BaseURL:      "https://flag.example/v1",
		Model:        "flag-model",
		Fast:         true,
		Medium:       true,
		Timeout:      "3s",
		MaxSteps:     2,
		AppendPrompt: "prefer parser scope",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "env-key" {
		t.Fatalf("APIKey = %q", cfg.APIKey)
	}
	if cfg.AuthMode != AuthModeAPIKey {
		t.Fatalf("AuthMode = %q", cfg.AuthMode)
	}
	if cfg.AuthAccountID != "" {
		t.Fatalf("AuthAccountID = %q", cfg.AuthAccountID)
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
	if cfg.AppendPrompt != "prefer parser scope" {
		t.Fatalf("AppendPrompt = %q", cfg.AppendPrompt)
	}
}

func TestResolveRequiresAuth(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("HOME", t.TempDir())

	if _, err := Resolve(Options{}); err == nil {
		t.Fatal("expected missing auth error")
	}
}

func TestResolveUsesChatGPTAuthFileByDefault(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "http://legacy.example/v1")
	t.Setenv("OPENAI_MODEL", "")
	writeCodexAuth(t, `{
		"auth_mode": "chatgpt",
		"tokens": {
			"access_token": "access-token",
			"account_id": "workspace-123"
		}
	}`)

	cfg, err := Resolve(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthMode != AuthModeChatGPT {
		t.Fatalf("AuthMode = %q", cfg.AuthMode)
	}
	if cfg.APIKey != "access-token" {
		t.Fatalf("APIKey = %q", cfg.APIKey)
	}
	if cfg.AuthAccountID != "workspace-123" {
		t.Fatalf("AuthAccountID = %q", cfg.AuthAccountID)
	}
	if cfg.BaseURL != DefaultChatGPTBaseURL {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
}

func TestResolveEmbeddingsRequiresAPIKeyEvenWithChatGPTAuthFile(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "http://legacy.example/v1")
	t.Setenv(EnvEmbeddingAPIKey, "")
	writeCodexAuth(t, `{
		"auth_mode": "chatgpt",
		"tokens": {
			"access_token": "access-token",
			"account_id": "workspace-123"
		}
	}`)

	_, err := ResolveEmbeddings(Options{})
	if err == nil || !strings.Contains(err.Error(), "search requires OPENAI_EMBEDDING_API_KEY or OPENAI_API_KEY") {
		t.Fatalf("expected API key error, got %v", err)
	}
}

func TestResolveEmbeddingsUsesEmbeddingOnlyEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "general-key")
	t.Setenv("OPENAI_BASE_URL", "http://general.example/v1")
	t.Setenv(EnvEmbeddingAPIKey, "embedding-key")
	t.Setenv(EnvEmbeddingBaseURL, "http://embedding.example/v1")
	t.Setenv(EnvEmbeddingModel, "text-embedding-3-large")
	t.Setenv(EnvEmbeddingDimensions, "512")

	cfg, err := ResolveEmbeddings(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "embedding-key" {
		t.Fatalf("APIKey = %q", cfg.APIKey)
	}
	if cfg.BaseURL != "http://embedding.example/v1" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if got := ResolveEmbeddingModel(""); got != "text-embedding-3-large" {
		t.Fatalf("embedding model = %q", got)
	}
	if got := ResolveEmbeddingModel("flag-model"); got != "flag-model" {
		t.Fatalf("flag embedding model = %q", got)
	}
	if got, err := ResolveEmbeddingDimensions(0); err != nil || got != 512 {
		t.Fatalf("embedding dimensions = %d, %v", got, err)
	}
	if got, err := ResolveEmbeddingDimensions(1024); err != nil || got != 1024 {
		t.Fatalf("flag embedding dimensions = %d, %v", got, err)
	}
}

func TestResolveEmbeddingDimensionsRejectsInvalidEnv(t *testing.T) {
	t.Setenv(EnvEmbeddingDimensions, "nope")

	_, err := ResolveEmbeddingDimensions(0)
	if err == nil || !strings.Contains(err.Error(), "invalid OPENAI_EMBEDDING_DIMENSIONS") {
		t.Fatalf("expected invalid dimensions error, got %v", err)
	}
}

func TestResolveIgnoresEmbeddingOnlyEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "general-key")
	t.Setenv("OPENAI_BASE_URL", "http://general.example/v1")
	t.Setenv("OPENAI_MODEL", "general-model")
	t.Setenv(EnvEmbeddingAPIKey, "embedding-key")
	t.Setenv(EnvEmbeddingBaseURL, "http://embedding.example/v1")
	t.Setenv(EnvEmbeddingModel, "text-embedding-3-large")

	cfg, err := Resolve(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIKey != "general-key" {
		t.Fatalf("APIKey = %q", cfg.APIKey)
	}
	if cfg.BaseURL != "http://general.example/v1" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.Model != "general-model" {
		t.Fatalf("Model = %q", cfg.Model)
	}
}

func TestResolveAllowsBaseURLFlagToOverrideChatGPTDefault(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("OPENAI_BASE_URL", "http://legacy.example/v1")
	writeCodexAuth(t, `{
		"auth_mode": "chatgpt",
		"tokens": {
			"access_token": "access-token",
			"account_id": "workspace-123"
		}
	}`)

	cfg, err := Resolve(Options{BaseURL: "http://flag.example/codex"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BaseURL != "http://flag.example/codex" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
}

func TestResolvePrefersChatGPTAuthFileOverAPIKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")
	t.Setenv("OPENAI_BASE_URL", "http://legacy.example/v1")
	t.Setenv("OPENAI_MODEL", "")
	writeCodexAuth(t, `{
		"auth_mode": "chatgpt",
		"tokens": {
			"access_token": "access-token",
			"account_id": "workspace-123"
		}
	}`)

	cfg, err := Resolve(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthMode != AuthModeChatGPT {
		t.Fatalf("AuthMode = %q", cfg.AuthMode)
	}
	if cfg.APIKey != "access-token" {
		t.Fatalf("APIKey = %q", cfg.APIKey)
	}
}

func TestResolveFallsBackToAPIKeyWhenAuthFileModeIsNotChatGPT(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "env-key")
	writeCodexAuth(t, `{
		"auth_mode": "apikey",
		"tokens": {
			"access_token": "access-token",
			"account_id": "workspace-123"
		}
	}`)

	cfg, err := Resolve(Options{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthMode != AuthModeAPIKey {
		t.Fatalf("AuthMode = %q", cfg.AuthMode)
	}
	if cfg.APIKey != "env-key" {
		t.Fatalf("APIKey = %q", cfg.APIKey)
	}
}

func TestResolveRejectsIncompleteChatGPTAuthFile(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	writeCodexAuth(t, `{
		"auth_mode": "chatgpt",
		"tokens": {
			"access_token": "access-token"
		}
	}`)

	_, err := Resolve(Options{})
	if err == nil || !strings.Contains(err.Error(), "missing tokens.account_id") {
		t.Fatalf("expected account id error, got %v", err)
	}
}

func TestResolveUsesRaisedDefaultMaxSteps(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "env-key")

	if _, err := Resolve(Options{Low: true, Medium: true}); err == nil {
		t.Fatal("expected mutually exclusive thinking flags error")
	}
}

func writeCodexAuth(t *testing.T, content string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
