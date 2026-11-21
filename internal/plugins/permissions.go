package plugins

import (
	"strings"

	"github.com/uniu-network/uniubot/internal/core/config"
)

// Permission modes.
const (
	// PermissionModeDeclared grants what the manifest declared, minus the deny list.
	PermissionModeDeclared = "declared"
	// PermissionModeSafe grants capabilities that are not sensitive (no db, no message.send).
	PermissionModeSafe = "safe"
	// PermissionModeStrict grants only capabilities listed in grants.<id>.
	PermissionModeStrict = "strict"
)

type Permissions struct {
	Mode   string
	Grants map[string][]string
	Deny   map[string][]string
}

// LoadPermissions reads plugins.permissions from the global configuration.
func LoadPermissions(global *config.Store) Permissions {
	policy := Permissions{
		Mode:   PermissionModeDeclared,
		Grants: map[string][]string{},
		Deny:   map[string][]string{},
	}
	if global == nil {
		return policy
	}
	if mode := strings.ToLower(global.String("plugins.permissions.mode", "")); mode != "" {
		policy.Mode = mode
	}
	grants := global.Map("plugins.permissions.grants")
	for id, raw := range grants {
		policy.Grants[id] = toStringSlice(raw)
	}
	deny := global.Map("plugins.permissions.deny")
	for id, raw := range deny {
		policy.Deny[id] = toStringSlice(raw)
	}
	return policy
}

func toStringSlice(raw any) []string {
	switch typed := raw.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, strings.TrimSpace(strings.ToLower(asString(item))))
		}
		return out
	case []string:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, strings.ToLower(item))
		}
		return out
	case string:
		if typed == "" {
			return nil
		}
		return []string{strings.ToLower(typed)}
	default:
		return nil
	}
}

func asString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return ""
}

// Resolve never grants a capability absent from the manifest, whatever the
// handshake declared.
func (p Permissions) Resolve(pluginID string, manifestCaps, declaredCaps []string) []string {
	allowed := map[string]bool{}
	for _, capability := range manifestCaps {
		allowed[capability] = true
	}
	denied := map[string]bool{}
	for _, capability := range p.Deny[pluginID] {
		denied[capability] = true
	}
	explicit := map[string]bool{}
	for _, capability := range p.Grants[pluginID] {
		explicit[capability] = true
	}

	out := []string{}
	seen := map[string]bool{}
	for _, capability := range declaredCaps {
		if !allowed[capability] || denied[capability] {
			continue
		}
		switch p.Mode {
		case PermissionModeStrict:
			if !explicit[capability] {
				continue
			}
		case PermissionModeSafe:
			if IsSensitiveCapability(capability) && !explicit[capability] {
				continue
			}
		}
		if !seen[capability] {
			seen[capability] = true
			out = append(out, capability)
		}
	}
	return out
}
