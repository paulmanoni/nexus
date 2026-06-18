// Binary: the demo app on the net/http.ServeMux backend (Go 1.22+). Zero
// third-party router deps — measures the floor of the seam's footprint.
package main

import (
	"net/http"

	"routerseam/app"
	"routerseam/httpx/stdrouter"
)

func main() {
	r := stdrouter.New()
	app.Register(r)
	http.ListenAndServe(":0", r)
}
