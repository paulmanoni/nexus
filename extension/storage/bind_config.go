package storage

import (
	"github.com/paulmanoni/nexus/v2"
	"github.com/paulmanoni/nexus/v2/config"
)

// BindFromConfig binds a marker type T to the [storage.<name>] block in
// nexus.toml — the storage counterpart to db.BindFromConfig, so local-in-dev
// / s3-in-prod becomes a config edit with no build closure to hand-write. T
// must embed *Manager, exactly as with Bind; lifecycle + dashboard
// registration are identical.
//
//	storage.BindFromConfig[Uploads]("uploads", storage.WithDefault())
//
// reads the block:
//
//	[storage.uploads]
//	driver          = "s3"          # "local" (default) | "s3"
//	root            = "./var/uploads"   # local
//	bucket          = "my-bucket"       # s3
//	region          = "us-east-1"
//	endpoint        = "https://minio.internal"   # s3-compatible host; empty = AWS
//	path_style      = true
//	access_key      = "${S3_ACCESS_KEY}"
//	secret_key      = "${S3_SECRET_KEY}"
//	session_token   = ""
//	public_base_url = "https://cdn.example.com"
//
// The build runs at boot (config.Get resolves the toml base layer, any config
// extension, and ENV overrides), so this works under nexus.Boot. Required
// fields for the chosen driver are still validated at boot by buildDisk.
func BindFromConfig[T any](name string, opts ...BindOption) nexus.Option {
	return Bind[T](name, func() Config { return configFromTOML(name) }, opts...)
}

func configFromTOML(name string) Config {
	p := "storage." + name + "."
	return Config{
		Driver:        config.Get(p+"driver", ""),
		Root:          config.Get(p+"root", ""),
		Bucket:        config.Get(p+"bucket", ""),
		Region:        config.Get(p+"region", ""),
		Endpoint:      config.Get(p+"endpoint", ""),
		PathStyle:     config.Get(p+"path_style", false),
		AccessKey:     config.Get(p+"access_key", ""),
		SecretKey:     config.Get(p+"secret_key", ""),
		SessionToken:  config.Get(p+"session_token", ""),
		PublicBaseURL: config.Get(p+"public_base_url", ""),
	}
}
