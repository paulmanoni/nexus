package nexus

import "runtime"

// runtimeFuncForPC is split out so the rest of reflect.go is easily testable
// without pulling in runtime internals. Returns the function's fully-qualified
// name, or "" if the PC doesn't resolve (happens for closures built via
// reflect.MakeFunc that we synthesize internally).
func runtimeFuncForPC(pc uintptr) string {
	f := runtime.FuncForPC(pc)
	if f == nil {
		return ""
	}
	return f.Name()
}
