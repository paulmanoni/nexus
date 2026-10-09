package orm

// SetDevMode switches on what the ORM does only under nexus dev.
func SetDevMode(t interface{ Cleanup(func()) }, on bool) {
	prev := devMode
	devMode = on
	t.Cleanup(func() { devMode = prev })
}
