package mail

import (
	"time"

	"github.com/paulmanoni/nexus/v2/config"
)

// tomlBlock is one [mail.<name>] block, read by BindFromConfig.
type tomlBlock struct {
	Driver      string        `toml:"driver"`
	Host        string        `toml:"host"`
	Port        int           `toml:"port"`
	Username    string        `toml:"username"`
	Password    string        `toml:"password"`
	Encryption  string        `toml:"encryption"`
	FromAddress string        `toml:"from_address"`
	FromName    string        `toml:"from_name"`
	Timeout     time.Duration `toml:"timeout"`
}

// Declares [mail.<name>] so nexus.toml may carry it.
var _ = config.Section[map[string]tomlBlock]("mail")
