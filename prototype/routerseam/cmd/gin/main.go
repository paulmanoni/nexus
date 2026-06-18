// Binary: the demo app on the gin backend. Exists to measure the compiled
// footprint of the seam + gin adapter vs the chi one.
package main

import (
	"net/http"

	"routerseam/app"
	"routerseam/httpx/ginrouter"
)

func main() {
	r := ginrouter.New()
	app.Register(r)
	http.ListenAndServe(":0", r)
}
