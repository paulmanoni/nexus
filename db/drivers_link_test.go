package db_test

// The driver split means package db links no engine; the test binary
// opts back in the way an app does.
import (
	_ "github.com/paulmanoni/nexus/v2/db/mysql"
	_ "github.com/paulmanoni/nexus/v2/db/postgres"
	_ "github.com/paulmanoni/nexus/v2/db/sqlite"
)
