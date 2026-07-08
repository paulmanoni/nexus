package db

import (
	"testing"

	gormlogger "gorm.io/gorm/logger"
)

func TestResolveLogLevel(t *testing.T) {
	cases := []struct {
		raw  string
		dev  bool
		want gormlogger.LogLevel
	}{
		// auto: quiet in prod, warn in dev
		{"", false, gormlogger.Silent},
		{"", true, gormlogger.Warn},
		{"  ", false, gormlogger.Silent},
		// explicit off — wins even in dev
		{"silent", true, gormlogger.Silent},
		{"false", true, gormlogger.Silent},
		{"off", true, gormlogger.Silent},
		// explicit on — wins even in prod (the opt-out)
		{"true", false, gormlogger.Warn},
		{"on", false, gormlogger.Warn},
		{"warn", false, gormlogger.Warn},
		{"error", false, gormlogger.Error},
		{"info", false, gormlogger.Info},
		{"all", false, gormlogger.Info},
		// unknown → auto
		{"nonsense", false, gormlogger.Silent},
		{"nonsense", true, gormlogger.Warn},
		// case-insensitive
		{"SILENT", true, gormlogger.Silent},
		{"Info", false, gormlogger.Info},
	}
	for _, c := range cases {
		if got := resolveLogLevel(c.raw, c.dev); got != c.want {
			t.Errorf("resolveLogLevel(%q, dev=%v) = %d, want %d", c.raw, c.dev, got, c.want)
		}
	}
}
