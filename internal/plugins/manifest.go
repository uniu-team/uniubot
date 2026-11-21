// Package plugins is the out-of-process plugin supervisor: discovery, manifest
// validation, process lifecycle and the host side RPC services.
package plugins

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// validPluginID keeps ids safe: they become database prefixes and config keys.
var validPluginID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type RestartPolicy struct {
	// MaxAttempts counts restarts after the initial start, clamped to 10.
	MaxAttempts int `json:"maxAttempts"`
	BackoffMS   int `json:"backoffMs"`
}

type Manifest struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Description  string            `json:"description"`
	Author       string            `json:"author"`
	License      string            `json:"license"`
	Protocol     string            `json:"protocol"`
	APIVersion   string            `json:"apiVersion"`
	Runtime      string            `json:"runtime"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	WorkDir      string            `json:"workdir"`
	Env          map[string]string `json:"env"`
	Enabled      *bool             `json:"enabled"`
	Capabilities []string          `json:"capabilities"`
	Concurrency  int               `json:"concurrency"`
	TimeoutMS    int               `json:"timeoutMs"`
	Restart      *RestartPolicy    `json:"restart"`
}

func (m *Manifest) SetDefaults() {
	m.Name = strings.TrimSpace(m.Name)
	if m.Name == "" {
		m.Name = m.ID
	}
	if m.Protocol == "" {
		m.Protocol = "jsonrpc-2.0-ndjson"
	}
	if m.APIVersion == "" {
		m.APIVersion = "v1"
	}
	if m.Version == "" {
		m.Version = "0.0.0"
	}
	if m.Concurrency <= 0 {
		m.Concurrency = 4
	}
	if m.Concurrency > 64 {
		m.Concurrency = 64
	}
	if m.TimeoutMS <= 0 {
		m.TimeoutMS = 5000
	}
	if m.Restart == nil {
		m.Restart = &RestartPolicy{MaxAttempts: 2, BackoffMS: 500}
	}
	if m.Restart.MaxAttempts < 0 {
		m.Restart.MaxAttempts = 0
	}
	if m.Restart.MaxAttempts > 10 {
		m.Restart.MaxAttempts = 10
	}
	if m.Restart.BackoffMS < 100 {
		m.Restart.BackoffMS = 100
	}
	if m.Restart.BackoffMS > 30000 {
		m.Restart.BackoffMS = 30000
	}
}

func (m *Manifest) EnabledOrDefault() bool {
	if m.Enabled == nil {
		return true
	}
	return *m.Enabled
}

func (m *Manifest) Timeout() time.Duration {
	if m.TimeoutMS <= 0 {
		return 5 * time.Second
	}
	return time.Duration(m.TimeoutMS) * time.Millisecond
}

func (m *Manifest) Validate() error {
	if m.LegacyManifest() {
		return fmt.Errorf("legacy in-process plugin manifest detected (index.ts/%s): the Go host only supports RPC plugins; see docs/plugin-system.md for the new plugin.json format", "plugin.json")
	}
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("manifest is missing the required \"id\" field")
	}
	if !validPluginID.MatchString(m.ID) {
		return fmt.Errorf("invalid plugin id %q: only letters, digits, '.', '_' and '-' are allowed", m.ID)
	}
	if m.Protocol != "jsonrpc-2.0-ndjson" {
		return fmt.Errorf("unsupported protocol %q: expected \"jsonrpc-2.0-ndjson\"", m.Protocol)
	}
	if m.APIVersion != "v1" {
		return fmt.Errorf("unsupported apiVersion %q: this host implements \"v1\"", m.APIVersion)
	}
	if strings.TrimSpace(m.Command) == "" {
		return fmt.Errorf("manifest is missing the required \"command\" field")
	}
	if len(m.Capabilities) != 0 {
		seen := map[string]bool{}
		for _, capability := range m.Capabilities {
			if !IsKnownCapability(capability) {
				return fmt.Errorf("unknown capability %q", capability)
			}
			if seen[capability] {
				return fmt.Errorf("duplicate capability %q", capability)
			}
			seen[capability] = true
		}
	}
	return nil
}

// LegacyManifest detects an old in-process plugin.json (no protocol/runtime/command).
func (m *Manifest) LegacyManifest() bool {
	return m.Protocol == "" && m.Runtime == "" && m.Command == "" && m.APIVersion == ""
}

func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return nil, fmt.Errorf("invalid JSON in %s: %w", path, err)
	}
	manifest.SetDefaults()
	return &manifest, nil
}

const (
	CapLogger       = "logger"
	CapConfigRead   = "config.read"
	CapBotInfo      = "bot.info"
	CapCommandList  = "command.list"
	CapMessageReply = "message.reply"
	CapMessageSend  = "message.send"
	CapEventPublish = "event.publish"
	CapDB           = "db"
)

var knownCapabilities = map[string]bool{
	CapLogger:       true,
	CapConfigRead:   true,
	CapBotInfo:      true,
	CapCommandList:  true,
	CapMessageReply: true,
	CapMessageSend:  true,
	CapEventPublish: true,
	CapDB:           true,
}

// sensitiveCapabilities need an explicit grant unless the mode is "declared".
var sensitiveCapabilities = map[string]bool{
	CapDB:          true,
	CapMessageSend: true,
}

func IsKnownCapability(capability string) bool { return knownCapabilities[capability] }

func IsSensitiveCapability(capability string) bool { return sensitiveCapabilities[capability] }
