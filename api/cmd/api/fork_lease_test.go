// SPDX-License-Identifier: Apache-2.0
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

func TestLooksLikeTarGz(t *testing.T) {
	// Build a real tar.gz.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "handler.py", Size: 5, Mode: 0o644})
	_, _ = tw.Write([]byte("print"))
	_ = tw.Close()
	_ = gz.Close()

	if !looksLikeTarGz(buf.Bytes()) {
		t.Fatal("valid tar.gz not detected")
	}
	// Raw single-file source (what create() stores) must NOT be detected.
	if looksLikeTarGz([]byte("print(\"hello\")\n")) {
		t.Fatal("raw source misdetected as tar.gz")
	}
	// Plain gzip that is not a tar must NOT be detected.
	var g bytes.Buffer
	gw := gzip.NewWriter(&g)
	_, _ = gw.Write([]byte("just some gzipped text, not a tar"))
	_ = gw.Close()
	if looksLikeTarGz(g.Bytes()) {
		t.Fatal("non-tar gzip misdetected as tar.gz")
	}
	if looksLikeTarGz(nil) || looksLikeTarGz([]byte{0x1f}) {
		t.Fatal("short/empty input misdetected as tar.gz")
	}
}
