package cache

import "github.com/paulmanoni/nexus/v2/extension/cache/cacheconfig"

// tomlBlock is one [cache.<name>] block, read by BindFromConfig; the table
// is declared in cacheconfig.
type tomlBlock = cacheconfig.Block
