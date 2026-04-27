package local

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// loadLocalConfig reads a YAML file at path into a generic map. Used
// to seed identity.Agent.Config + extract the agent.* identity
// fields. Returns (nil, nil) if path is empty — useful for tests
// that construct Source with no file.
func loadLocalConfig(path string) (map[string]any, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// YAML is optional in local mode; missing file ≡
			// empty config map. Identity fields fall back to
			// the "local" stub defaults.
			return nil, nil
		}
		return nil, fmt.Errorf("identity local: read %s: %w", path, err)
	}
	var config map[string]any
	if err := yaml.Unmarshal(f, &config); err != nil {
		return nil, fmt.Errorf("identity local: parse %s: %w", path, err)
	}
	return config, nil
}

// stringFromMap pulls a string value at key from m, returning
// fallback when missing or the wrong type.
func stringFromMap(m map[string]any, key, fallback string) string {
	if m == nil {
		return fallback
	}
	v, ok := m[key]
	if !ok {
		return fallback
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return fallback
	}
	return s
}
