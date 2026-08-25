// SPDX-License-Identifier: Apache-2.0
package firecracker

import (
	"os"
	"testing"
)

func TestNoHugeTemplate(t *testing.T) {
	t.Setenv("PANDASTACK_NOHUGE_TEMPLATES", "base-8g, other-tpl, app-*")
	cases := map[string]bool{
		"base-8g":       true,
		"other-tpl":     true, // trimmed
		"app-1234-cafe": true, // trailing-* prefix pattern
		"app-":          true, // prefix itself matches
		"application":   false,
		"base":          false,
		"":              false, // empty template never matches
	}
	for tpl, want := range cases {
		if got := noHugeTemplate(tpl); got != want {
			t.Errorf("noHugeTemplate(%q)=%v want %v", tpl, got, want)
		}
	}
	os.Unsetenv("PANDASTACK_NOHUGE_TEMPLATES")
	if noHugeTemplate("base-8g") {
		t.Error("env unset must disable the class")
	}
}
