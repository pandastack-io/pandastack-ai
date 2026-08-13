// SPDX-License-Identifier: Apache-2.0
package main

import (
	"reflect"
	"testing"
)

func TestParseForkChildIDs(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"cold/warm fork", `{"parent":"p","children":["a","b"],"at":"now"}`, []string{"a", "b"}},
		{"fork-tree", `{"tree_id":"t","children":[{"id":"x","guest_ip":"1.2.3.4"},{"id":"y"}]}`, []string{"x", "y"}},
		{"empty children", `{"children":[]}`, nil},
		{"missing children", `{"parent":"p"}`, nil},
		{"garbage", `not json`, nil},
		{"blank ids filtered", `{"children":["","z"]}`, []string{"z"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseForkChildIDs([]byte(tc.body))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseForkChildIDs(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}
