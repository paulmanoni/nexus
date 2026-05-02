package cache

import "github.com/paulmanoni/nexus/manifest"

// ManifestEnv lists the env vars this package's NewConfig reads. Apps
// that include cache.Module in their nexus.Run options should also
// add nexus.DeclareEnvList(cache.ManifestEnv()) so the deploy-time
// manifest reflects the cache's configuration surface — otherwise an
// orchestration platform reading the manifest won't know REDIS_HOST /
// REDIS_PORT / REDIS_PASSWORD / APP_ENV are required.
//
// Two design notes:
//
//  1. We expose a slice rather than implementing manifest.EnvProvider
//     on *Manager. EnvProvider would only be detected once *Manager
//     enters the graph, but in print mode we want declarations
//     visible WITHOUT firing constructors (Manager's constructor
//     dials Redis). Static slices are side-effect-free by definition.
//
//  2. We expose ManifestService too — the cache's logical sidecar
//     ("redis"). Both are returned together as a convenience but kept
//     as separate functions so an app that, say, uses an external
//     managed Redis (no provisioned sidecar needed) can include
//     ManifestEnv() but skip ManifestService().
func ManifestEnv() []manifest.EnvVar {
	return []manifest.EnvVar{
		{
			Name:        "APP_ENV",
			Description: "Environment label used to flip cache layer behavior (development/staging/production)",
			Default:     "development",
		},
		{
			Name:        "REDIS_HOST",
			Description: "Redis hostname; cache falls back to in-memory when absent",
			BoundTo:     "redis.host",
		},
		{
			Name:        "REDIS_PORT",
			Description: "Redis port",
			Default:     "6379",
			BoundTo:     "redis.port",
		},
		{
			Name:        "REDIS_PASSWORD",
			Description: "Redis password (omit for unauthenticated dev Redis)",
			Secret:      true,
			BoundTo:     "redis.password",
		},
	}
}

// ManifestService describes the Redis sidecar this package would like
// the orchestration platform to provision. Optional=true reflects
// that the cache layer degrades to in-memory when Redis is absent —
// the platform can skip provisioning in dev environments where that
// trade-off is acceptable.
func ManifestService() manifest.ServiceNeed {
	return manifest.ServiceNeed{
		Name:     "redis",
		Kind:     "redis",
		Optional: true,
		ExposeAs: map[string]string{
			"host":     "REDIS_HOST",
			"port":     "REDIS_PORT",
			"password": "REDIS_PASSWORD",
		},
	}
}