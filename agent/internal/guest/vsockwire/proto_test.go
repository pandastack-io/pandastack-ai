// SPDX-License-Identifier: Apache-2.0

package vsockwire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"testing"
)

func TestRoundTripExec(t *testing.T) {
	var buf bytes.Buffer
	req := ExecRequest{Cmd: "echo hi", TimeoutMS: 5000}
	if err := WriteFrame(&buf, OpExec, req); err != nil {
		t.Fatalf("write: %v", err)
	}
	op, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if op != OpExec {
		t.Fatalf("op = %v, want %v", op, OpExec)
	}
	var got ExecRequest
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Cmd != req.Cmd || got.TimeoutMS != req.TimeoutMS {
		t.Fatalf("got %+v, want %+v", got, req)
	}
}

func TestRoundTripBinaryFileData(t *testing.T) {
	var buf bytes.Buffer
	// Include NUL and high bytes to confirm base64 ([]byte) survives framing.
	data := []byte{0x00, 0xff, 'a', 0x10, '\n', 0x80}
	if err := WriteFrame(&buf, OpReadFile, ReadFileResponse{Data: data}); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got ReadFileResponse
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !bytes.Equal(got.Data, data) {
		t.Fatalf("data = %v, want %v", got.Data, data)
	}
}

func TestBadMagic(t *testing.T) {
	buf := bytes.NewReader([]byte{'X', 'Y', byte(OpExec), 0, 0, 0, 0})
	if _, _, err := ReadFrame(buf); err != ErrBadMagic {
		t.Fatalf("err = %v, want ErrBadMagic", err)
	}
}

func TestPayloadTooLargeOnRead(t *testing.T) {
	hdr := make([]byte, headerLen)
	hdr[0] = Magic[0]
	hdr[1] = Magic[1]
	hdr[2] = byte(OpExec)
	// length field = MaxPayload+1
	binary.BigEndian.PutUint32(hdr[3:], uint32(MaxPayload)+1)
	if _, _, err := ReadFrame(bytes.NewReader(hdr)); err != ErrPayloadTooLarge {
		t.Fatalf("err = %v, want ErrPayloadTooLarge", err)
	}
}

func TestTruncatedPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, OpStat, StatRequest{Path: "/etc/hostname"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Drop the last 3 bytes to simulate a truncated stream.
	full := buf.Bytes()
	truncated := full[:len(full)-3]
	if _, _, err := ReadFrame(bytes.NewReader(truncated)); err != io.ErrUnexpectedEOF {
		t.Fatalf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestMultipleFramesSequential(t *testing.T) {
	var buf bytes.Buffer
	_ = WriteFrame(&buf, OpHello, HelloRequest{Version: ProtocolVersion})
	_ = WriteFrame(&buf, OpStat, StatRequest{Path: "/tmp"})

	op1, _, err := ReadFrame(&buf)
	if err != nil || op1 != OpHello {
		t.Fatalf("frame1 op=%v err=%v", op1, err)
	}
	op2, p2, err := ReadFrame(&buf)
	if err != nil || op2 != OpStat {
		t.Fatalf("frame2 op=%v err=%v", op2, err)
	}
	var sr StatRequest
	_ = json.Unmarshal(p2, &sr)
	if sr.Path != "/tmp" {
		t.Fatalf("path = %q", sr.Path)
	}
}
