// Package mcp implements the client side of the Model Context Protocol.
package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Config is the MCP portion of a notch configuration file.
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

type OAuthConfig struct {
	Scope string `json:"scope,omitempty"`
}

// ServerConfig describes either a stdio server (Command) or a Streamable HTTP
// server (URL). Enabled defaults to true when omitted from JSON.
type ServerConfig struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Auth    string            `json:"auth,omitempty"`
	OAuth   *OAuthConfig      `json:"oauth,omitempty"`
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

func (c ServerConfig) OAuthEnabled() bool { return c.OAuth != nil || c.Auth == "oauth" }

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
	if err := cfg.expandEnvironment(os.LookupEnv); err != nil {
		return Config{}, fmt.Errorf("parse MCP config %q: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) expandEnvironment(lookup func(string) (string, bool)) error {
	for name, server := range c.MCPServers {
		if !server.isEnabled() {
			continue
		}
		var err error
		server.Env, err = expandMap(server.Env, lookup)
		if err != nil {
			return fmt.Errorf("server %q env: %w", name, err)
		}
		server.Headers, err = expandMap(server.Headers, lookup)
		if err != nil {
			return fmt.Errorf("server %q headers: %w", name, err)
		}
		c.MCPServers[name] = server
	}
	return nil
}

func expandMap(values map[string]string, lookup func(string) (string, bool)) (map[string]string, error) {
	for key, value := range values {
		expanded, err := expandEnvironment(value, lookup)
		if err != nil {
			return nil, fmt.Errorf("%q: %w", key, err)
		}
		values[key] = expanded
	}
	return values, nil
}

func expandEnvironment(value string, lookup func(string) (string, bool)) (string, error) {
	var result strings.Builder
	for i := 0; i < len(value); {
		if value[i] != '$' {
			result.WriteByte(value[i])
			i++
			continue
		}
		if i+1 < len(value) && value[i+1] == '$' {
			result.WriteByte('$')
			i += 2
			continue
		}
		if i+1 >= len(value) || value[i+1] != '{' {
			result.WriteByte('$')
			i++
			continue
		}
		endOffset := strings.IndexByte(value[i+2:], '}')
		if endOffset < 0 {
			return "", fmt.Errorf("unterminated environment variable reference")
		}
		end := i + 2 + endOffset
		name := value[i+2 : end]
		if !validEnvironmentName(name) {
			return "", fmt.Errorf("invalid environment variable reference")
		}
		expanded, ok := lookup(name)
		if !ok {
			return "", fmt.Errorf("environment variable %q is not set", name)
		}
		result.WriteString(expanded)
		i = end + 1
	}
	return result.String(), nil
}

func validEnvironmentName(name string) bool {
	if name == "" || !isEnvironmentNameStart(name[0]) {
		return false
	}
	for i := 1; i < len(name); i++ {
		if !isEnvironmentNameStart(name[i]) && (name[i] < '0' || name[i] > '9') {
			return false
		}
	}
	return true
}

func isEnvironmentNameStart(char byte) bool {
	return char == '_' || char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z'
}
