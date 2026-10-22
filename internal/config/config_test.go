package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The example configs shipped in the repo must always load.
func TestShippedConfigsLoad(t *testing.T) {
	for _, name := range []string{"config.yaml", "config.real.yaml"} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Load(filepath.Join("..", "..", name))
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Providers) == 0 {
				t.Error("no providers")
			}
		})
	}
}

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDefaultsApplied(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
providers:
  - name: m
    type: mock
    models: [{name: x, context_window: 1000}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.Policy != "cost" || cfg.Retry.MaxAttempts != 3 || cfg.Server.RequestTimeout != 30*time.Second {
		t.Errorf("defaults not applied: %+v", cfg)
	}
}

func TestInvalidConfigs(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"typo in key", "providerz: []", "field providerz not found"},
		{"no providers", "server: {addr: ':1'}", "at least one provider"},
		{"unknown policy", `
routing: {policy: random}
providers: [{name: m, type: mock, models: [{name: x, context_window: 1}]}]`, "routing.policy"},
		{"real provider without key env", `
providers: [{name: a, type: anthropic, models: [{name: x, context_window: 1}]}]`, "api_key_env is required"},
		{"rule references unknown model", `
routing: {policy: rule, rules: [{name: r, prefer: [nope/x]}]}
providers: [{name: m, type: mock, models: [{name: x, context_window: 1}]}]`, `unknown model "nope/x"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
