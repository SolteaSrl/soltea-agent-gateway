// Package config carica e valida il config.json dell'agent-runner.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// Project mappa un progetto Project-Open a una cartella locale sulla VM.
type Project struct {
	ProjectID int    `json:"project_id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
}

// Config e' il contenuto di config.json (vedi DESIGN.md §7).
type Config struct {
	GatewayURL       string `json:"gateway_url"`
	AgentID          string `json:"agent_id"`
	Token            string `json:"token"`
	ClaudePath       string `json:"claude_path"`
	UseGitBash       bool   `json:"use_git_bash"`
	GitBashPath      string `json:"git_bash_path"`
	DefaultModel     string `json:"default_model"`
	PermissionMode   string `json:"permission_mode"`
	HeartbeatSeconds int    `json:"heartbeat_seconds"`
	// LogDir: cartella dei log del runner. Se vuoto, default = cartella "logs"
	// accanto al config.json (risolto in Load).
	LogDir   string    `json:"log_dir"`
	Projects []Project `json:"projects"`
}

// Load legge e valida il file di configurazione.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lettura config %q: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}
	c.applyDefaults()
	c.resolveLogDir(path)
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.ClaudePath == "" {
		c.ClaudePath = "claude.exe"
	}
	if c.PermissionMode == "" {
		c.PermissionMode = "acceptEdits"
	}
	if c.HeartbeatSeconds <= 0 {
		c.HeartbeatSeconds = 30
	}
	if c.GitBashPath == "" {
		c.GitBashPath = `C:\Program Files\Git\bin\bash.exe`
	}
}

// resolveLogDir rende LogDir un percorso assoluto. Se non specificato, usa la
// cartella "logs" accanto al config.json (NON la working dir, che per il
// servizio Windows e' system32).
func (c *Config) resolveLogDir(configPath string) {
	dir := c.LogDir
	if dir == "" {
		if abs, err := filepath.Abs(configPath); err == nil {
			dir = filepath.Join(filepath.Dir(abs), "logs")
		} else {
			dir = "logs"
		}
	} else if !filepath.IsAbs(dir) {
		if abs, err := filepath.Abs(dir); err == nil {
			dir = abs
		}
	}
	c.LogDir = dir
}

func (c *Config) validate() error {
	if c.GatewayURL == "" {
		return fmt.Errorf("gateway_url mancante")
	}
	u, err := url.Parse(c.GatewayURL)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") {
		return fmt.Errorf("gateway_url deve essere ws:// o wss:// (ho: %q)", c.GatewayURL)
	}
	if c.AgentID == "" {
		return fmt.Errorf("agent_id mancante")
	}
	if c.Token == "" {
		return fmt.Errorf("token mancante")
	}
	if len(c.Projects) == 0 {
		return fmt.Errorf("nessun progetto dichiarato in 'projects'")
	}
	for i, p := range c.Projects {
		if p.ProjectID == 0 || p.Path == "" {
			return fmt.Errorf("projects[%d]: project_id e path sono obbligatori", i)
		}
	}
	return nil
}

// ProjectPath ritorna la cartella locale per un project_id dichiarato.
func (c *Config) ProjectPath(projectID int) (string, bool) {
	for _, p := range c.Projects {
		if p.ProjectID == projectID {
			return p.Path, true
		}
	}
	return "", false
}

// ProjectsForHello serializza i progetti per il frame hello.
func (c *Config) ProjectsForHello() []map[string]any {
	out := make([]map[string]any, 0, len(c.Projects))
	for _, p := range c.Projects {
		out = append(out, map[string]any{"project_id": p.ProjectID, "name": p.Name, "path": p.Path})
	}
	return out
}

// HTTPBase ricava la base HTTP(S) per i blob dall'URL WebSocket.
// wss://host/agents/ws -> https://host/agents
func (c *Config) HTTPBase() string {
	u, err := url.Parse(c.GatewayURL)
	if err != nil {
		return ""
	}
	switch u.Scheme {
	case "wss":
		u.Scheme = "https"
	case "ws":
		u.Scheme = "http"
	}
	u.Path = strings.TrimSuffix(u.Path, "/ws")
	return strings.TrimSuffix(u.String(), "/")
}
