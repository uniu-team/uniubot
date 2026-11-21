// Package args is the command line parser exposed to plugins and commands.
// Dashed keys are camelCased: --dry-run becomes dryRun.
package args

import (
	"encoding/json"
	"regexp"
	"strings"
)

type Args struct {
	Positional []string
	// Flags keeps insertion order for JSON output.
	Flags map[string]any
	order []string
}

func New() *Args {
	return &Args{Positional: []string{}, Flags: map[string]any{}}
}

func Parse(tokens []string) *Args {
	parsed := New()
	i := 0
	for i < len(tokens) {
		token := tokens[i]
		switch {
		case strings.HasPrefix(token, "--") && len(token) > 2:
			i += parsed.parseLong(token, tokens, i)
		case strings.HasPrefix(token, "-") && len(token) > 1 && !isNegativeNumber(token):
			i += parsed.parseShort(token, tokens, i)
		default:
			parsed.Positional = append(parsed.Positional, token)
			i++
		}
	}
	return parsed
}

func ParseLine(raw string) *Args {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return New()
	}
	return Parse(strings.Fields(trimmed))
}

var negativeNumber = regexp.MustCompile(`^-\d`)

func isNegativeNumber(token string) bool {
	return negativeNumber.MatchString(token)
}

func (a *Args) set(key string, value any) {
	if _, exists := a.Flags[key]; !exists {
		a.order = append(a.order, key)
	}
	a.Flags[key] = value
}

func (a *Args) parseLong(token string, tokens []string, idx int) int {
	if eqIdx := strings.Index(token, "="); eqIdx > 2 {
		a.set(normalizeKey(token[2:eqIdx]), token[eqIdx+1:])
		return 1
	}
	rawKey := token[2:]
	if strings.HasPrefix(rawKey, "no-") && len(rawKey) > 3 {
		a.set(normalizeKey(rawKey[3:]), false)
		return 1
	}
	key := normalizeKey(rawKey)
	if idx+1 < len(tokens) && !strings.HasPrefix(tokens[idx+1], "-") {
		a.set(key, tokens[idx+1])
		return 2
	}
	a.set(key, true)
	return 1
}

func (a *Args) parseShort(token string, tokens []string, idx int) int {
	flags := token[1:]
	if len(flags) == 1 {
		if idx+1 < len(tokens) && !strings.HasPrefix(tokens[idx+1], "-") {
			a.set(flags, tokens[idx+1])
			return 2
		}
	}
	for _, ch := range flags {
		a.set(string(ch), true)
	}
	return 1
}

func normalizeKey(key string) string {
	var b strings.Builder
	upperNext := false
	for _, r := range key {
		if r == '-' {
			upperNext = true
			continue
		}
		if upperNext {
			b.WriteString(strings.ToUpper(string(r)))
			upperNext = false
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func (a *Args) GetString(key string) string {
	if value, ok := a.Flags[key]; ok {
		if text, ok := value.(string); ok {
			return text
		}
	}
	return ""
}

func (a *Args) Get(key string) (any, bool) {
	value, ok := a.Flags[key]
	return value, ok
}

func (a *Args) Bool(key string) bool {
	value, ok := a.Flags[key].(bool)
	return ok && value
}

func (a *Args) String(key, def string) string {
	if value, ok := a.Flags[key].(string); ok {
		return value
	}
	return def
}

func (a *Args) FirstPositional() string {
	if len(a.Positional) > 0 {
		return a.Positional[0]
	}
	return ""
}

// MarshalJSON emits the flat object plugins receive: positionals under "_".
func (a *Args) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(a.Flags)+1)
	out["_"] = a.Positional
	for key, value := range a.Flags {
		out[key] = value
	}
	return json.Marshal(out)
}

func (a *Args) UnmarshalJSON(raw []byte) error {
	var flat map[string]any
	if err := json.Unmarshal(raw, &flat); err != nil {
		return err
	}
	a.Flags = map[string]any{}
	a.order = a.order[:0]
	if positional, ok := flat["_"].([]any); ok {
		a.Positional = make([]string, 0, len(positional))
		for _, item := range positional {
			a.Positional = append(a.Positional, stringify(item))
		}
		delete(flat, "_")
	} else if positional, ok := flat["_"].([]string); ok {
		a.Positional = append([]string{}, positional...)
		delete(flat, "_")
	}
	if a.Positional == nil {
		a.Positional = []string{}
	}
	for key, value := range flat {
		a.set(key, value)
	}
	sortStrings(a.order)
	return nil
}

func stringify(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return strings.Trim(string(encoded), `"`)
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
