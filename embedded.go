// Package embedded compiles this repository's own config.json and
// models.json into the binary, so one shipped on its own — no repo checkout
// on the host — still boots with a complete configuration and model ladder.
//
// The embed directive cannot reach outside the directory of the file that
// declares it, which is why this lives at the module root next to the files
// it embeds rather than under cmd/ or internal/: neither of those can reach
// ../config.json or ../models.json.
//
// An external config.json or models.json found next to the running binary,
// or passed via --config, always takes precedence over these — see
// internal/config.Load and internal/models.Load, which decide when each
// applies.
package embedded

import _ "embed"

//go:embed config.json
var Config []byte

//go:embed models.json
var Models []byte
