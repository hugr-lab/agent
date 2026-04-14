package file

import (
	"github.com/fsnotify/fsnotify"
	"github.com/hugr-lab/hugen/interfaces"
	"github.com/spf13/viper"
)

// ConfigProvider wraps viper with the ConfigProvider interface and hot reload.
type ConfigProvider struct {
	v *viper.Viper
}

var _ interfaces.ConfigProvider = (*ConfigProvider)(nil)

// NewConfigProvider creates a config provider from a YAML file.
// If configPath is empty, only env vars are used.
func NewConfigProvider(configPath string) (*ConfigProvider, error) {
	v := viper.New()
	v.SetConfigType("yaml")
	v.AutomaticEnv()

	if configPath != "" {
		v.SetConfigFile(configPath)
		if err := v.ReadInConfig(); err != nil {
			// Config file is optional — env vars still work.
			if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
				return nil, err
			}
		}
		v.WatchConfig()
	}

	return &ConfigProvider{v: v}, nil
}

func (c *ConfigProvider) Get(key string) any              { return c.v.Get(key) }
func (c *ConfigProvider) GetString(key string) string     { return c.v.GetString(key) }
func (c *ConfigProvider) GetInt(key string) int           { return c.v.GetInt(key) }
func (c *ConfigProvider) GetFloat64(key string) float64   { return c.v.GetFloat64(key) }

func (c *ConfigProvider) OnChange(callback func()) {
	c.v.OnConfigChange(func(_ fsnotify.Event) { callback() })
}
