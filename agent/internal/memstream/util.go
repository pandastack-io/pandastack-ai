// SPDX-License-Identifier: Apache-2.0
package memstream

import (
	"encoding/json"
	"io"
)

// decodeJSON decodes a single JSON value from r into v. Kept local so the
// package has no dependency surface beyond the standard library.
func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}
