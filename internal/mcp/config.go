// Package mcp implements the client side of the Model Context Protocol.
package mcp

import (
	"encoding/json"
	"fmt"
	"os"
)

// Config is the MCP portion of a notch configuration file.
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// ServerConfig describes either a stdio server (Command) or a Streamable HTTP
// server (URL). Enabled defaults to true when omitted from JSON.
type ServerConfig struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Enabled bool              `json:"enabled,omitempty"`

	enabledSet bool
}

func (c *ServerConfig) UnmarshalJSON(data []byte) error {
	type plain ServerConfig
	var v struct {
		plain
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*c = ServerConfig(v.plain)
	if v.Enabled != nil {
		c.Enabled = *v.Enabled
		c.enabledSet = true
	}
	return nil
}

func (c ServerConfig) isEnabled() bool { return !c.enabledSet || c.Enabled }

// LoadConfig reads an MCP configuration from path.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read MCP config %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse MCP config %q: %w", path, err)
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = make(map[string]ServerConfig)
	}
	return cfg, nil
}
