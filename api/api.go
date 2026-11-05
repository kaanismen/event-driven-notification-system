// Package api holds the OpenAPI specification of the HTTP API. The file is compiled into
// the binary, so the served spec always matches the code it was built with.
package api

import _ "embed" // the blank import enables the //go:embed directive below

// Spec is openapi.yaml, served at GET /openapi.yaml.
//
//go:embed openapi.yaml
var Spec []byte
