// SPDX-License-Identifier: Apache-2.0
package migrations

import "embed"

// FS contains goose SQL migrations for both supported database dialects.
//
//go:embed sqlite/*.sql postgres/*.sql
var FS embed.FS
