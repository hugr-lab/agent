package agent

// Config is the YAML-static agent section (`agent:` in config.yaml).
// Runtime wiring (Router, Sessions, Tokens, …) lives in Runtime; this
// type carries only load-time file-system settings.
type Config struct {
	Constitution string `mapstructure:"constitution"`
}
