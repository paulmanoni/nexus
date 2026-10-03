package storage

import "github.com/paulmanoni/nexus/v2/config"

// tomlBlock is one [storage.<name>] block, read by BindFromConfig.
type tomlBlock struct {
	Driver        string `toml:"driver"`
	Root          string `toml:"root"`
	Bucket        string `toml:"bucket"`
	Region        string `toml:"region"`
	Endpoint      string `toml:"endpoint"`
	PathStyle     bool   `toml:"path_style"`
	AccessKey     string `toml:"access_key"`
	SecretKey     string `toml:"secret_key"`
	SessionToken  string `toml:"session_token"`
	PublicBaseURL string `toml:"public_base_url"`
}

// Declares [storage.<name>] so nexus.toml may carry it.
var _ = config.Section[map[string]tomlBlock]("storage")
