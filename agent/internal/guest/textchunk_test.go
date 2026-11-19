// SPDX-License-Identifier: Apache-2.0
package guest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// The reproduced case: three thousand repeats of a 2-, a 2- and a 3-byte
// character, read from the guest 4096 bytes at a time. 4096 is not a multiple
// of the 7-byte pattern, so most reads end inside a character.
func TestTextChunkerKeepsSplitCharactersWhole(t *testing.T) {
	want := strings.Repeat("مр世", 3000) + "\n"
	raw := []byte(want)

	for _, readSize := range []int{1, 2, 3, 5, 7, 4096, 32 * 1024} {
		var tc TextChunker
		var got strings.Builder
		for off := 0; off < len(raw); off += readSize {
			end := min(off+readSize, len(raw))
			chunk := tc.Push(raw[off:end])
			if !utf8.Valid(chunk) {
				t.Fatalf("read size %d: chunk at %d is not valid UTF-8", readSize, off)
			}
			// What the handler does with a chunk.
			b, _ := json.Marshal(map[string]string{"chunk": string(chunk)})
			var back struct {
				Chunk string `json:"chunk"`
			}
			_ = json.Unmarshal(b, &back)
			got.WriteString(back.Chunk)
		}
		got.Write(tc.Flush())
		if got.String() != want {
			t.Fatalf("read size %d: output changed (%d replacement characters)", readSize, strings.Count(got.String(), "�"))
		}
	}
}

// Without the chunker the same reads lose characters. This is the bug, kept
// as a test so the reason for TextChunker stays checkable.
func TestRawReadsLoseSplitCharacters(t *testing.T) {
	raw := []byte(strings.Repeat("مр世", 3000))
	var got strings.Builder
	for off := 0; off < len(raw); off += 4096 {
		b, _ := json.Marshal(map[string]string{"chunk": string(raw[off:min(off+4096, len(raw))])})
		var back struct {
			Chunk string `json:"chunk"`
		}
		_ = json.Unmarshal(b, &back)
		got.WriteString(back.Chunk)
	}
	if !strings.Contains(got.String(), "�") {
		t.Fatal("expected raw 4096-byte reads to produce U+FFFD; the premise of TextChunker no longer holds")
	}
}

func TestTextChunkerPassesBinaryThrough(t *testing.T) {
	// Bytes that can never become valid UTF-8 are not held back, and nothing
	// is lost or reordered whatever the input.
	inputs := [][]byte{
		{0xff, 0xfe, 0x00, 0x80, 0x80, 0x80, 0x80, 0x80},
		{0xc0, 0x41},             // invalid lead byte, then ASCII
		{0xe4, 0x41, 0xe4, 0xb8}, // a lead byte cut short by ASCII, then a real unfinished tail
		bytes.Repeat([]byte{0x80}, 9),
	}
	for _, in := range inputs {
		for size := 1; size <= len(in); size++ {
			var tc TextChunker
			var got []byte
			for off := 0; off < len(in); off += size {
				got = append(got, tc.Push(in[off:min(off+size, len(in))])...)
			}
			got = append(got, tc.Flush()...)
			if !bytes.Equal(got, in) {
				t.Fatalf("input % x, read size %d: got % x", in, size, got)
			}
		}
	}
}

func TestTextChunkerHoldsAtMostThreeBytes(t *testing.T) {
	var tc TextChunker
	// The first three bytes of a four-byte character (U+1F600).
	if out := tc.Push([]byte{'a', 0xf0, 0x9f, 0x98}); string(out) != "a" {
		t.Fatalf("got %q, want only the complete prefix", out)
	}
	if len(tc.carry) != 3 {
		t.Fatalf("held %d bytes, want 3", len(tc.carry))
	}
	if out := tc.Push([]byte{0x80, 'b'}); string(out) != "\U0001F600b" {
		t.Fatalf("got %q after the last byte arrived", out)
	}
	if rest := tc.Flush(); len(rest) != 0 {
		t.Fatalf("flush returned % x, want nothing", rest)
	}
}
