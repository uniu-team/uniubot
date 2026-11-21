package plugins

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Discovered struct {
	Dir          string
	ManifestPath string
	Manifest     *Manifest
	Err          error
}

// Discover finds plugins/<name>/plugin.json and one nested level, as used by
// plugins/examples/<name>/.
func Discover(pluginDir string) []Discovered {
	entries, err := os.ReadDir(pluginDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	nested := map[string][]string{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		names = append(names, entry.Name())
		dir := filepath.Join(pluginDir, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, "plugin.json")); err == nil {
			continue
		}
		children, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, child := range children {
			if !child.IsDir() {
				continue
			}
			childDir := filepath.Join(dir, child.Name())
			if _, err := os.Stat(filepath.Join(childDir, "plugin.json")); err == nil {
				nested[entry.Name()] = append(nested[entry.Name()], child.Name())
			}
		}
	}
	sort.Strings(names)

	out := make([]Discovered, 0, len(names))
	for _, name := range names {
		dir := filepath.Join(pluginDir, name)
		manifestPath := filepath.Join(dir, "plugin.json")
		if _, err := os.Stat(manifestPath); err != nil {
			for _, child := range nested[name] {
				childDir := filepath.Join(dir, child)
				if item, ok := loadDiscovered(childDir); ok {
					out = append(out, item)
				}
			}
			continue
		}
		item, _ := loadDiscovered(dir)
		out = append(out, item)
	}
	return out
}

func loadDiscovered(dir string) (Discovered, bool) {
	manifestPath := filepath.Join(dir, "plugin.json")
	if _, err := os.Stat(manifestPath); err != nil {
		return Discovered{}, false
	}
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return Discovered{Dir: dir, ManifestPath: manifestPath, Err: err}, true
	}
	discovered := Discovered{Dir: dir, ManifestPath: manifestPath, Manifest: manifest}
	if err := manifest.Validate(); err != nil {
		discovered.Err = err
	}
	return discovered, true
}

func findPluginDir(pluginDir, id string) (string, bool) {
	for _, item := range Discover(pluginDir) {
		if item.Manifest != nil && item.Manifest.ID == id {
			return item.Dir, true
		}
		if strings.EqualFold(filepath.Base(item.Dir), id) {
			return item.Dir, true
		}
	}
	return "", false
}
