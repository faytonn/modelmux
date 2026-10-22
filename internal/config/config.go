// Package config loads and validates the gateway's YAML configuration.
// API keys are deliberately not part of the file: a provider names the
// environment variable that holds its key instead.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Server    Server     `yaml:"server"`
	Providers []Provider `yaml:"providers"`
}

type Server struct {
	Addr             string        `yaml:"addr"`
	RequestTimeout   time.Duration `yaml:"request_timeout"` // budget for the whole request, fallbacks included
	DefaultMaxTokens int           `yaml:"default_max_tokens"`
}

type Provider struct {
	Name           string        `yaml:"name"`
	Type           string        `yaml:"type"` // mock | anthropic | openai
	BaseURL        string        `yaml:"base_url"`
	APIKeyEnv      string        `yaml:"api_key_env"`
	AttemptTimeout time.Duration `yaml:"attempt_timeout"` // budget for a single upstream call
	Mock           Mock          `yaml:"mock"`
	Models         []Model       `yaml:"models"`
}

type Mock struct {
	Latency       time.Duration `yaml:"latency"`
	Jitter        time.Duration `yaml:"jitter"`
	ErrorRate     float64       `yaml:"error_rate"`
	RateLimitRate float64       `yaml:"rate_limit_rate"`
	RetryAfter    time.Duration `yaml:"retry_after"`
}

type Model struct {
	Name             string  `yaml:"name"`
	ContextWindow    int     `yaml:"context_window"` // tokens
	InputPricePer1K  float64 `yaml:"input_price_per_1k"`
	OutputPricePer1K float64 `yaml:"output_price_per_1k"`
	Quality          int     `yaml:"quality"` // relative rank, higher is better
}

// Cost estimates the USD cost of a call with the given token counts.
func (m Model) Cost(inputTokens, outputTokens int) float64 {
	return float64(inputTokens)/1000*m.InputPricePer1K + float64(outputTokens)/1000*m.OutputPricePer1K
}

// Load reads, defaults and validates the config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // a typo in a key is an error, not a silently ignored field
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.Server.RequestTimeout == 0 {
		c.Server.RequestTimeout = 30 * time.Second
	}
	if c.Server.DefaultMaxTokens == 0 {
		c.Server.DefaultMaxTokens = 512
	}
	for i := range c.Providers {
		if c.Providers[i].AttemptTimeout == 0 {
			c.Providers[i].AttemptTimeout = 10 * time.Second
		}
	}
}

func (c *Config) validate() error {
	var errs []error
	if len(c.Providers) == 0 {
		errs = append(errs, errors.New("at least one provider is required"))
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			errs = append(errs, errors.New("provider with empty name"))
		}
		if seen[p.Name] {
			errs = append(errs, fmt.Errorf("duplicate provider %q", p.Name))
		}
		seen[p.Name] = true
		switch p.Type {
		case "mock":
		default:
			errs = append(errs, fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type))
		}
		if len(p.Models) == 0 {
			errs = append(errs, fmt.Errorf("provider %q: no models", p.Name))
		}
		for _, m := range p.Models {
			if m.Name == "" || m.ContextWindow <= 0 {
				errs = append(errs, fmt.Errorf("provider %q: every model needs a name and a positive context_window", p.Name))
			}
		}
	}
	// errors.Join reports every problem at once instead of one per restart.
	return errors.Join(errs...)
}
