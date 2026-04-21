package skills

// Config is the skills YAML section (`skills:` in config.yaml). Path
// is the on-disk root where per-skill `SKILL.md` + `memory.yaml`
// files are discovered by FileManager.
type Config struct {
	Path string `mapstructure:"path"`
}
