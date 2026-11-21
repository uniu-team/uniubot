package plugins

import (
	"sort"
	"sync"
	"time"

	"github.com/uniu-network/uniubot/pkg/pluginrpc"
)

type State string

// Lifecycle states; DISABLED marks a plugin configured off.
const (
	StateDiscovered   State = "DISCOVERED"
	StateValidating   State = "VALIDATING"
	StateStarting     State = "STARTING"
	StateInitializing State = "INITIALIZING"
	StateRegistering  State = "REGISTERING"
	StateReady        State = "READY"
	StateStopping     State = "STOPPING"
	StateStopped      State = "STOPPED"
	StateDisabled     State = "DISABLED"
	StateFailed       State = "FAILED"
	StateCrashed      State = "CRASHED"
	StateUnavailable  State = "UNAVAILABLE"
)

type SchemaInfo struct {
	Fields []pluginrpc.ConfigField `json:"fields"`
}

type Entry struct {
	ID          string
	Name        string
	Version     string
	Description string
	Author      string
	Runtime     string
	Dir         string
	Manifest    *Manifest

	State  State
	Reason string

	Commands             []pluginrpc.CommandSpec
	Methods              []string
	DeclaredCapabilities []string
	GrantedCapabilities  []string
	ConfigSchema         []pluginrpc.ConfigField
	Events               []string

	PID           int
	StartedAt     time.Time
	LastError     string
	RestartCount  int
	ProtocolError int64
}

// Info is the API representation of a plugin.
type Info struct {
	// Name is the plugin id and the plugin_config.<name> key: keep it stable.
	Name         string                  `json:"name"`
	ID           string                  `json:"id"`
	Description  string                  `json:"description"`
	Version      string                  `json:"version"`
	Author       string                  `json:"author"`
	Runtime      string                  `json:"runtime"`
	State        string                  `json:"state"`
	IsEnabled    bool                    `json:"isEnabled"`
	IsLoaded     bool                    `json:"isLoaded"`
	ConfigSchema *SchemaInfo             `json:"configSchema"`
	Commands     []pluginrpc.CommandSpec `json:"commands,omitempty"`
	Capabilities []string                `json:"capabilities,omitempty"`
	Reason       string                  `json:"reason,omitempty"`
	PID          int                     `json:"pid,omitempty"`
}

// Registry is safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

func NewRegistry() *Registry {
	return &Registry{entries: map[string]*Entry{}}
}

func (r *Registry) Upsert(entry Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copyEntry := entry
	r.entries[entry.ID] = &copyEntry
}

// Update mutates an entry under the registry lock.
func (r *Registry) Update(id string, fn func(*Entry)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.entries[id]; ok {
		fn(entry)
	}
}

// Get returns a copy, so callers may mutate it freely.
func (r *Registry) Get(id string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.entries[id]
	if !ok {
		return Entry{}, false
	}
	return *entry, true
}

func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, id)
}

func (r *Registry) Snapshot() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.entries))
	for _, entry := range r.entries {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

func (r *Registry) Ready() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []string{}
	for id, entry := range r.entries {
		if entry.State == StateReady {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
