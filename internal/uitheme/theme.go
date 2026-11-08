// Package uitheme owns the design primitives shared by both web applications.
package uitheme

import _ "embed"

// CSS is static build-owned content, never user input.
//
//go:embed assets/tokens.css
var CSS string
