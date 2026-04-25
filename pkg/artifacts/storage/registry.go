package storage

import (
	"fmt"
	"sync"
)

// Factory builds a Storage from a backend-specific config value. The
// runtime (pkg/runtime) calls Open with the right substruct
// (cfg.Artifacts.FS for "fs", cfg.Artifacts.S3 for "s3"); cfg is
// type-asserted by the factory.
type Factory func(cfg any) (Storage, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register adds a backend factory under name. Panics on duplicate
// registration — catches boot-time programmer errors.
//
// Per Constitution §III, this is called explicitly from
// pkg/runtime/runtime.go at boot, NOT from init() in the backend
// packages. The registry is process-global because there's exactly
// one runtime per process; tests can flush via reset for isolation.
func Register(name string, f Factory) {
	if name == "" {
		panic("artifacts/storage: Register: empty name")
	}
	if f == nil {
		panic("artifacts/storage: Register: nil factory for " + name)
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("artifacts/storage: Register: duplicate backend " + name)
	}
	registry[name] = f
}

// Open builds a Storage from the named backend's config. Returns a
// clear "unknown backend" error when name was never Register'd —
// pkg/runtime turns this into a fatal boot error per US8 / FR-024.
func Open(name string, cfg any) (Storage, error) {
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("artifacts/storage: unknown backend %q", name)
	}
	s, err := f(cfg)
	if err != nil {
		return nil, fmt.Errorf("artifacts/storage: open %q: %w", name, err)
	}
	return s, nil
}

// reset clears the registry. Test-only.
func reset() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]Factory{}
}
