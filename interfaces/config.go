package interfaces

// ConfigProvider abstracts configuration with hot reload support.
type ConfigProvider interface {
	// Get returns a config value by key.
	Get(key string) any

	// GetString returns a string config value.
	GetString(key string) string

	// GetInt returns an int config value.
	GetInt(key string) int

	// OnChange registers a callback invoked when config changes.
	OnChange(callback func())
}
