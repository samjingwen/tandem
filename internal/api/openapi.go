package api

import _ "embed"

// The hand-authored v1 wire contract is embedded so discovery matches the
// executable revision, without depending on a source checkout at runtime.
//
//go:embed openapi.json
var openAPIDocument []byte
