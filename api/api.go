// Package api holds the OpenPersona API contract: the HTTP API as an
// OpenAPI document (openapi.yaml) and the gRPC services as Protobuf
// (proto/openpersona/v1), with the Go code generated from them in gen.
// See README.md in this directory for the compatibility rules.
package api

import _ "embed"

// OpenAPI is the OpenAPI 3.1 document of the HTTP API.
//
//go:embed openapi.yaml
var OpenAPI []byte
