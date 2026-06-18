package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	AuthModeAPIKey        = "api_key"
	AuthModeChatGPT       = "chatgpt"
	DefaultBaseURL        = "https://api.openai.com/v1"
	DefaultChatGPTBaseURL = "https://chatgpt.com/backend-api/codex"
	DefaultModel          = "gpt-5.4"
	DefaultTimeout        = 2 * time.Minute
	DefaultMaxSteps       = 30
	DefaultMaxTools       = 24
	defaultCodexAuthPath  = ".codex/auth.json"
)

const (
	DefaultEmbeddingModel      = "text-embedding-3-small"
	DefaultEmbeddingDimensions = 1024
)

const (
	EnvEmbeddingAPIKey     = "OPENAI_EMBEDDING_API_KEY"
	EnvEmbeddingBaseURL    = "OPENAI_EMBEDDING_BASE_URL"
	EnvEmbeddingModel      = "OPENAI_EMBEDDING_MODEL"
	EnvEmbeddingDimensions = "OPENAI_EMBEDDING_DIMENSIONS"
)

type Config struct {
	APIKey         string
	AuthMode       string
	AuthAccountID  string
	BaseURL        string
	Model          string
	ServiceTier    string
	ThinkingEffort string
	Timeout        time.Duration
	MaxSteps       int
	MaxToolCalls   int
	GuidanceFamily string
	AppendPrompt   string
	Debug          bool
}

type EmbeddingsConfig struct {
	APIKey  string
	BaseURL string
	Timeout time.Duration
	Debug   bool
}

type Options struct {
	APIKey         string
	BaseURL        string
	Model          string
	Fast           bool
	Low            bool
	Medium         bool
	High           bool
	XHigh          bool
	Timeout        string
	MaxSteps       int
	GuidanceFamily string
	AppendPrompt   string
	Debug          bool
}

type codexAuthFile struct {
	AuthMode string `json:"auth_mode"`
	Tokens   *struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

func Resolve(opts Options) (Config, error) {
	timeout := DefaultTimeout
	if opts.Timeout != "" {
		parsed, err := time.ParseDuration(opts.Timeout)
		if err != nil {
			return Config{}, fmt.Errorf("invalid --timeout: %w", err)
		}
		if parsed <= 0 {
			return Config{}, errors.New("--timeout must be positive")
		}
		timeout = parsed
	}

	maxSteps := DefaultMaxSteps
	if opts.MaxSteps != 0 {
		if opts.MaxSteps < 1 {
			return Config{}, errors.New("--max-steps must be positive")
		}
		maxSteps = opts.MaxSteps
	}
	thinkingModeFlags := 0
	for _, enabled := range []bool{opts.Low, opts.Medium, opts.High, opts.XHigh} {
		if enabled {
			thinkingModeFlags++
		}
	}
	if thinkingModeFlags > 1 {
		return Config{}, errors.New("--low, --medium, --high, and --xhigh are mutually exclusive")
	}

	auth, err := resolveAuth(opts)
	if err != nil {
		return Config{}, err
	}

	thinkingEffort := ""
	switch {
	case opts.Low:
		thinkingEffort = "low"
	case opts.Medium:
		thinkingEffort = "medium"
	case opts.XHigh:
		thinkingEffort = "xhigh"
	case opts.High:
		thinkingEffort = "high"
	}

	serviceTier := ""
	if opts.Fast {
		serviceTier = "priority"
	}

	return Config{
		APIKey:         auth.apiKey,
		AuthMode:       auth.mode,
		AuthAccountID:  auth.accountID,
		BaseURL:        resolveBaseURL(opts.BaseURL, auth),
		Model:          firstNonEmpty(opts.Model, os.Getenv("OPENAI_MODEL"), DefaultModel),
		ServiceTier:    serviceTier,
		ThinkingEffort: thinkingEffort,
		Timeout:        timeout,
		MaxSteps:       maxSteps,
		MaxToolCalls:   DefaultMaxTools,
		GuidanceFamily: firstNonEmpty(opts.GuidanceFamily, stringDefaultGuidanceFamily()),
		AppendPrompt:   opts.AppendPrompt,
		Debug:          opts.Debug,
	}, nil
}

func ResolveEmbeddings(opts Options) (EmbeddingsConfig, error) {
	timeout := DefaultTimeout
	if opts.Timeout != "" {
		parsed, err := time.ParseDuration(opts.Timeout)
		if err != nil {
			return EmbeddingsConfig{}, fmt.Errorf("invalid --timeout: %w", err)
		}
		if parsed <= 0 {
			return EmbeddingsConfig{}, errors.New("--timeout must be positive")
		}
		timeout = parsed
	}

	apiKey := firstNonEmpty(opts.APIKey, os.Getenv(EnvEmbeddingAPIKey), os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return EmbeddingsConfig{}, errors.New("search requires OPENAI_EMBEDDING_API_KEY or OPENAI_API_KEY; Codex auth does not support embeddings")
	}
	return EmbeddingsConfig{
		APIKey:  apiKey,
		BaseURL: firstNonEmpty(opts.BaseURL, os.Getenv(EnvEmbeddingBaseURL), os.Getenv("OPENAI_BASE_URL"), DefaultBaseURL),
		Timeout: timeout,
		Debug:   opts.Debug,
	}, nil
}

func ResolveEmbeddingModel(flagValue string) string {
	return firstNonEmpty(flagValue, os.Getenv(EnvEmbeddingModel), DefaultEmbeddingModel)
}

func ResolveEmbeddingDimensions(flagValue int) (int, error) {
	if flagValue != 0 {
		if flagValue < 1 {
			return 0, errors.New("--embedding-dimensions must be positive")
		}
		return flagValue, nil
	}
	if value := os.Getenv(EnvEmbeddingDimensions); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, fmt.Errorf("invalid %s: %w", EnvEmbeddingDimensions, err)
		}
		if parsed < 1 {
			return 0, fmt.Errorf("%s must be positive", EnvEmbeddingDimensions)
		}
		return parsed, nil
	}
	return DefaultEmbeddingDimensions, nil
}

func resolveBaseURL(flagValue string, auth resolvedAuth) string {
	if flagValue != "" {
		return flagValue
	}
	if auth.mode == AuthModeChatGPT {
		return auth.defaultBaseURL
	}
	return firstNonEmpty(os.Getenv("OPENAI_BASE_URL"), auth.defaultBaseURL)
}

type resolvedAuth struct {
	mode           string
	apiKey         string
	accountID      string
	defaultBaseURL string
}

func resolveAuth(opts Options) (resolvedAuth, error) {
	auth, err := readCodexAuth()
	if err == nil {
		if auth.AuthMode == AuthModeChatGPT {
			if auth.Tokens == nil || auth.Tokens.AccessToken == "" {
				return resolvedAuth{}, fmt.Errorf("%s missing tokens.access_token for chatgpt auth", defaultCodexAuthPath)
			}
			if auth.Tokens.AccountID == "" {
				return resolvedAuth{}, fmt.Errorf("%s missing tokens.account_id for chatgpt auth", defaultCodexAuthPath)
			}
			return resolvedAuth{
				mode:           AuthModeChatGPT,
				apiKey:         auth.Tokens.AccessToken,
				accountID:      auth.Tokens.AccountID,
				defaultBaseURL: DefaultChatGPTBaseURL,
			}, nil
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return resolvedAuth{}, err
	}

	apiKey := firstNonEmpty(opts.APIKey, os.Getenv("OPENAI_API_KEY"))
	if apiKey != "" {
		return resolvedAuth{mode: AuthModeAPIKey, apiKey: apiKey, defaultBaseURL: DefaultBaseURL}, nil
	}
	return resolvedAuth{}, errors.New("missing ~/.codex/auth.json and OPENAI_API_KEY")
}

func readCodexAuth() (codexAuthFile, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return codexAuthFile{}, fmt.Errorf("locate %s: %w", defaultCodexAuthPath, err)
	}
	data, err := os.ReadFile(filepath.Join(home, defaultCodexAuthPath))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return codexAuthFile{}, os.ErrNotExist
		}
		return codexAuthFile{}, fmt.Errorf("read %s: %w", defaultCodexAuthPath, err)
	}
	var auth codexAuthFile
	if err := json.Unmarshal(data, &auth); err != nil {
		return codexAuthFile{}, fmt.Errorf("parse %s: %w", defaultCodexAuthPath, err)
	}
	return auth, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func stringDefaultGuidanceFamily() string {
	return "auto"
}
