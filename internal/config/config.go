// Package config loads gateway configuration from environment variables.
//
// The gateway is intentionally minimal: it reads the upstream URL and
// listen address from the environment, validates them, and exposes them
// through a single struct. Upstream credentials are not config — they
// live in the backend registry (see package backend). Anything more
// elaborate (flag parsing, config files, hot reload) is out of scope.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

const (
	// EnvAnthropicBaseURL points the proxy at the Anthropic upstream.
	// Default is the production endpoint; tests may override it.
	EnvAnthropicBaseURL = "ANTHROPIC_BASE_URL"

	// EnvListenAddr is the loopback address the proxy binds to.
	// Default is 127.0.0.1:8080; intentionally loopback-only.
	EnvListenAddr = "LISTEN_ADDR"

	// DefaultBaseURL is the Anthropic production endpoint.
	DefaultBaseURL = "https://api.anthropic.com"

	// DefaultListenAddr is the loopback address used when LISTEN_ADDR
	// is unset. Loopback-only is intentional; see the README.
	DefaultListenAddr = "127.0.0.1:8080"
)

// Config is the resolved gateway configuration.
type Config struct {
	// AnthropicBaseURL is the upstream scheme + host the proxy forwards
	// to. The path is appended at request time.
	AnthropicBaseURL string

	// ListenAddr is the loopback address the gateway binds to.
	ListenAddr string
}

// Load reads the gateway configuration from the process environment.
//
// Returns an error if any value is malformed so a misconfigured
// deployment fails fast at startup. Credentials are loaded separately by
// the backend registry.
func Load() (Config, error) {
	cfg := Config{
		AnthropicBaseURL: getEnv(EnvAnthropicBaseURL, DefaultBaseURL),
		ListenAddr:       getEnv(EnvListenAddr, DefaultListenAddr),
	}

	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validate enforces the contract: an upstream URL with a scheme and
// host, and a loopback listen address.
func (c Config) validate() error {
	upstream, err := url.Parse(c.AnthropicBaseURL)
	if err != nil {
		return fmt.Errorf("ANTHROPIC_BASE_URL is invalid: %w", err)
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return fmt.Errorf("ANTHROPIC_BASE_URL=%q: scheme and host are required", c.AnthropicBaseURL)
	}
	if err := validateListenAddr(c.ListenAddr); err != nil {
		return err
	}
	return nil
}

// validateListenAddr enforces the loopback-only constraint: the host
// part of LISTEN_ADDR must be a literal loopback address (127.0.0.1,
// ::1) or the loopback name "localhost". We accept the name because it
// is the conventional shortcut and resolves to a loopback IP, but we
// reject 0.0.0.0 and any routable address — exposing the proxy
// off-loopback would put backend credentials on the wire.
func validateListenAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("LISTEN_ADDR is invalid: %w", err)
	}
	switch strings.ToLower(host) {
	case "localhost", "127.0.0.1", "::1":
		return nil
	default:
		return fmt.Errorf("LISTEN_ADDR must be loopback (127.0.0.1, ::1, or localhost); got %q", host)
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
