// SPDX-License-Identifier: Apache-2.0
package main

// openapi_serve.go — serves the OpenAPI 3.1 description of the public API.
// The spec is embedded at build time so the binary is self-describing; the
// drift test in openapi_drift_test.go keeps it honest against the registered
// routes.

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openapiSpec []byte

func registerOpenAPIRoute(mux *http.ServeMux) {
	serve := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Access-Control-Allow-Origin", "*") // spec is public; lets hosted explorers fetch it
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(openapiSpec)
	}
	mux.HandleFunc("GET /openapi.json", serve)
	mux.HandleFunc("GET /v1/openapi.json", serve)
}
