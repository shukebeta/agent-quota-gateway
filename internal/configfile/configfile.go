// Package configfile loads gateway and backend configuration from a JSON
// file. It provides precedence resolution (flag > env > default path),
// permission checks (0600), and decoding into the config.Inputs and
// backend.Spec types that both paths share.
package configfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/shukebeta/agent-quota-gateway/internal/backend"
	"github.com/shukebeta/agent-quota-gateway/internal/config"
)

const (
	// EnvConfigPath is the environment variable that holds the path to the
	// config file. The --config flag takes precedence.
	EnvConfigPath = "AQG_CONFIG"

	// DefaultConfigPath is the file name looked for in the current working
	// directory when neither the flag nor AQG_CONFIG are set.
	DefaultConfigPath = "aqg.json"
)

// Resolve implements the precedence order for config file discovery:
// (1) the --config flag value if non-empty; (2) AQG_CONFIG env if set;
// (3) ./aqg.json if it exists; (4) otherwise use env vars (useFile=false).
func Resolve(flagVal string) (path string, useFile bool) {
	// Flag takes highest precedence.
	if flagVal != "" {
		return flagVal, true
	}

	// Next, the env var.
	if envPath, ok := os.LookupEnv(EnvConfigPath); ok && envPath != "" {
		return envPath, true
	}

	// Finally, the default path in the current directory.
	if fi, err := os.Stat(DefaultConfigPath); err == nil && !fi.IsDir() {
		return DefaultConfigPath, true
	}

	// No file found; fall back to env.
	return "", false
}

// LoadFile reads and decodes a JSON config file from path, returning the
// resolved config.Config and backend.Registry. It fails closed on any
// error (unreadable file, wrong permissions, malformed JSON, validation
// failure). No env fallback — the caller must have already decided to
// use the file path via Resolve.
func LoadFile(path string) (config.Config, *backend.Registry, error) {
	// Check file permissions: must be 0600 (no group/other access).
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config.Config{}, nil, fmt.Errorf("config file %q does not exist", path)
		}
		return config.Config{}, nil, fmt.Errorf("config file %q: %w", path, err)
	}
	mode := fi.Mode().Perm()
	if mode&0o077 != 0 {
		return config.Config{}, nil, fmt.Errorf("config file %q must be 0600 (no group/other access); has mode %#o", path, mode)
	}

	// Read and decode JSON.
	f, err := os.Open(path)
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("config file %q: %w", path, err)
	}
	defer f.Close()

	var dto fileDTO
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields() // fail closed on typos
	if err := dec.Decode(&dto); err != nil {
		return config.Config{}, nil, fmt.Errorf("config file %q: %w", path, err)
	}

	// Map DTO to config.Inputs.
	cfgInputs := config.Inputs{
		AnthropicBaseURL: dto.BaseURL,
		ListenAddr:       dto.ListenAddr,
		SharedListenAddr: dto.SharedListenAddr,
		StateFile:        dto.StateFile,
	}
	cfg, err := config.Build(cfgInputs)
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("config: %w", err)
	}

	// Map DTO to backend.Spec.
	spec := backend.Spec{Pools: make(map[string]backend.PoolSpec, len(dto.Pools))}
	for poolKey, poolDTO := range dto.Pools {
		poolSpec := backend.PoolSpec{
			BaseURL:      poolDTO.BaseURL,
			Members:      make(map[string]backend.MemberSpec, len(poolDTO.Members)),
			Priority:     poolDTO.Priority,
			Balance:      poolDTO.Balance,
			BalanceGap:   poolDTO.BalanceGap,
			BalanceDwell: backend.Duration{D: poolDTO.BalanceDwell.D},
		}
		for nickKey, memberDTO := range poolDTO.Members {
			poolSpec.Members[nickKey] = backend.MemberSpec{
				Credential: memberDTO.Credential,
				BaseURL:    memberDTO.BaseURL,
			}
		}
		spec.Pools[poolKey] = poolSpec
	}

	registry, err := backend.BuildFromSpec(spec, cfg.AnthropicBaseURL)
	if err != nil {
		return config.Config{}, nil, fmt.Errorf("backend spec: %w", err)
	}

	return cfg, registry, nil
}

// fileDTO is the JSON shape of a config file. All fields are optional
// except the required structure (at least one pool with at least one member).
// Empty strings for BaseURL/Members[].BaseURL mean "use default".
type fileDTO struct {
	// BaseURL is the upstream URL. Empty string uses the gateway default.
	BaseURL string `json:"base_url"`

	// ListenAddr is the loopback bind address. Empty string uses the default.
	ListenAddr string `json:"listen_addr"`

	// SharedListenAddr opts into shared mode (Tailscale binding).
	SharedListenAddr string `json:"shared_listen_addr"`

	// StateFile is the path for the persistent state file. Empty disables it.
	StateFile string `json:"state_file"`

	// Pools maps pool names to their specs.
	Pools map[string]poolDTO `json:"pools"`
}

// poolDTO is one pool's configuration from the file.
type poolDTO struct {
	BaseURL      string               `json:"base_url"`
	Members      map[string]memberDTO `json:"members"`
	Priority     []string             `json:"priority"`
	Balance      string               `json:"balance"`
	BalanceGap   float64              `json:"balance_gap"`
	BalanceDwell backend.Duration     `json:"balance_dwell"`
}

// memberDTO is one backend's credential and optional base URL override.
type memberDTO struct {
	Credential string `json:"credential"`
	BaseURL    string `json:"base_url"`
}
