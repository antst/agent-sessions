// Package protocol owns the universal session wire authority.
package protocol

import _ "embed"

//go:embed session.schema.json
var SessionSchema []byte
