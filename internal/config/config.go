package config

import (
	"errors"
	"fmt"
	"os"
	"time"
)

const (
	DefaultBaseURL  = "https://api.openai.com/v1"
	DefaultModel    = "gpt-5.4"
	DefaultTimeout  = 2 * time.Minute
	DefaultMaxSteps = 30
	DefaultMaxTools = 24
)

type Config struct {
	APIKey         string
	BaseURL        string
	Model          string
	Timeout        time.Duration
	MaxSteps       int
	MaxToolCalls   int
	GuidanceFamily string
	Debug          bool
}

type Options struct {
	APIKey         string
	BaseURL        string
	Model          string
	Timeout        string
	MaxSteps       int
	GuidanceFamily string
	Debug          bool
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

	apiKey := firstNonEmpty(opts.APIKey, os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return Config{}, errors.New("missing OPENAI_API_KEY")
	}

	return Config{
		APIKey:         apiKey,
		BaseURL:        firstNonEmpty(opts.BaseURL, os.Getenv("OPENAI_BASE_URL"), DefaultBaseURL),
		Model:          firstNonEmpty(opts.Model, os.Getenv("OPENAI_MODEL"), DefaultModel),
		Timeout:        timeout,
		MaxSteps:       maxSteps,
		MaxToolCalls:   DefaultMaxTools,
		GuidanceFamily: firstNonEmpty(opts.GuidanceFamily, stringDefaultGuidanceFamily()),
		Debug:          opts.Debug,
	}, nil
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
