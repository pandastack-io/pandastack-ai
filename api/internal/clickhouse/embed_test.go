// SPDX-License-Identifier: Apache-2.0
package clickhouse

import (
	"strings"
	"testing"
)

// The embedded schema must carry the W1 byte/country ALTERs — the whole point
// of embedding was that these reach the edge api. And the CREATE must precede
// the ALTERs, since EnsureSchema runs statements in order and an ALTER on a
// not-yet-created table would error and halt the rest.
func TestEmbeddedSchemaHasW1Columns(t *testing.T) {
	if SchemaDDL == "" {
		t.Fatal("SchemaDDL embed is empty")
	}
	for _, col := range []string{"bytes_out", "bytes_in", "country"} {
		if !strings.Contains(SchemaDDL, col) {
			t.Fatalf("embedded schema missing ADD COLUMN %s", col)
		}
	}
	createIdx := strings.Index(SchemaDDL, "CREATE TABLE IF NOT EXISTS pandastack.http_requests")
	alterIdx := strings.Index(SchemaDDL, "ALTER TABLE pandastack.http_requests ADD COLUMN IF NOT EXISTS bytes_out")
	if createIdx < 0 || alterIdx < 0 || createIdx > alterIdx {
		t.Fatalf("http_requests CREATE must precede its ALTERs (create=%d alter=%d)", createIdx, alterIdx)
	}
}
