package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// config is the credentials file. It holds API tokens, so it is written
// with owner-only permissions.
type config struct {
	Default    string                    `json:"default,omitempty"`
	Registries map[string]registryConfig `json:"registries"`
}

type registryConfig struct {
	Token string `json:"token"`
}

func configPath(env Env) (string, error) {
	if p := env.Getenv("GOPHERDEX_CONFIG"); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find your config directory: %w (set GOPHERDEX_CONFIG)", err)
	}
	return filepath.Join(dir, "gopherdex", "credentials.json"), nil
}

func loadConfig(env Env) (*config, string, error) {
	path, err := configPath(env)
	if err != nil {
		return nil, "", err
	}
	cfg := &config{Registries: map[string]registryConfig{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, path, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, "", fmt.Errorf("%s is not valid JSON: %w", path, err)
	}
	if cfg.Registries == nil {
		cfg.Registries = map[string]registryConfig{}
	}
	return cfg, path, nil
}

func saveConfig(path string, cfg *config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".credentials-*.json")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
