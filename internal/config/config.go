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
	ServiceTier    string
	ThinkingEffort string
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
	Fast           bool
	Low            bool
	Medium         bool
	High           bool
	XHigh          bool
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
	thinkingModeFlags := 0
	for _, enabled := range []bool{opts.Low, opts.Medium, opts.High, opts.XHigh} {
		if enabled {
			thinkingModeFlags++
		}
	}
	if thinkingModeFlags > 1 {
		return Config{}, errors.New("--low, --medium, --high, and --xhigh are mutually exclusive")
	}

	apiKey := firstNonEmpty(opts.APIKey, os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return Config{}, errors.New("missing OPENAI_API_KEY")
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
		APIKey:         apiKey,
		BaseURL:        firstNonEmpty(opts.BaseURL, os.Getenv("OPENAI_BASE_URL"), DefaultBaseURL),
		Model:          firstNonEmpty(opts.Model, os.Getenv("OPENAI_MODEL"), DefaultModel),
		ServiceTier:    serviceTier,
		ThinkingEffort: thinkingEffort,
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
