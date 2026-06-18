// Binary: the demo app on the chi backend. Same app.Register, different
// router — and a far smaller binary.
package main

import (
	"net/http"

	"routerseam/app"
	"routerseam/httpx/chirouter"
)

func main() {
	r := chirouter.New()
	app.Register(r)
	http.ListenAndServe(":0", r)
}
