// SPDX-License-Identifier: Apache-2.0
package guest

import "unicode/utf8"

// TextChunker cuts a byte stream into pieces that never end in the middle of
// a UTF-8 character.
//
// Command output is read from the guest in fixed-size reads and each read is
// sent to the client as a JSON string. A read can end between the bytes of one
// character; encoding/json then replaces the stranded bytes on both sides of
// the cut with U+FFFD and the character is lost. TextChunker holds back the
// incomplete tail (at most three bytes) until the rest arrives.
//
// Bytes that are not UTF-8 at all pass through unchanged: only a tail that
// could still become a valid character is held. Use one TextChunker per
// stream; it is not safe for concurrent use.
type TextChunker struct {
	carry []byte
}

// Push adds data and returns the longest run that ends on a character
// boundary. The result may be empty; it does not alias data.
func (t *TextChunker) Push(data []byte) []byte {
	buf := make([]byte, 0, len(t.carry)+len(data))
	buf = append(buf, t.carry...)
	buf = append(buf, data...)
	n := completePrefix(buf)
	t.carry = append(t.carry[:0], buf[n:]...)
	return buf[:n]
}

// Flush returns whatever is still held. Call it once the stream has ended: a
// tail that never completed is real output and must not be dropped.
func (t *TextChunker) Flush() []byte {
	out := t.carry
	t.carry = nil
	return out
}

// completePrefix is the length of the longest prefix of b that does not stop
// inside a UTF-8 sequence.
func completePrefix(b []byte) int {
	// The start of an unfinished character is at most UTFMax-1 bytes from the
	// end. Walk back over continuation bytes to the byte that starts the last
	// character.
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if b[i]&0xC0 == 0x80 {
			continue // continuation byte
		}
		if utf8.FullRune(b[i:]) {
			return len(b) // complete, or invalid (which will never complete)
		}
		return i
	}
	return len(b) // nothing but continuation bytes: not UTF-8, pass it on
}
