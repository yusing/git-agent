package config

import "time"

type Config struct {
	APIKey         string
	BaseURL        string
	Model          string
	Timeout        time.Duration
	MaxSteps       int
	GuidanceFamily string
	Debug          bool
}
