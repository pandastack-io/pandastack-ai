// SPDX-License-Identifier: Apache-2.0

// Package vsockwire defines the framed request/response wire protocol spoken
// between the host-side guest.Client vsock fast-path and the in-guest
// pandastack-daemon. It is a deliberately small, length-prefixed binary
// protocol carrying JSON payloads.
//
// Frame layout (all integers big-endian):
//
//	+--------+--------+------------------+----------------------+
//	| magic  | opcode |  payload length  |       payload        |
//	| 2 byte | 1 byte |     4 byte u32   |    <length> bytes    |
//	+--------+--------+------------------+----------------------+
//
// magic is the constant Magic ("PD") so a peer can fail fast on a garbage
// stream. opcode selects the operation (see the Op* constants). The payload
// is a JSON-encoded request or response struct, chosen by opcode. payload
// length is capped at MaxPayload to bound memory on a hostile/garbled peer.
//
// The protocol is strictly request/response, one in-flight op per connection:
// the daemon accepts a connection, reads exactly one request frame, writes
// exactly one response frame, and closes. The host fast-path opens a fresh
// vsock connection per op. There is no multiplexing in Phase 1 (PTY and
// streaming exec stay on SSH); the stream-ID field is reserved for Phase 2 by
// keeping the framing extensible (new opcodes), not by encoding stream IDs
// now — keeping the v1 frame minimal.
package vsockwire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Magic is the 2-byte frame preamble ("PD" for PandaStack daemon).
var Magic = [2]byte{'P', 'D'}

// ProtocolVersion is advertised in the Hello handshake. Host and daemon must
// agree on the major version; a mismatch makes the host fall back to SSH.
const ProtocolVersion = 1

// DaemonPort is the fixed AF_VSOCK port the guest daemon listens on. Chosen
// to avoid collision with pandastack-init's existing ready port and any
// app/user ports. The host reaches it by dialing the per-sandbox FC vsock
// UDS and writing "CONNECT <DaemonPort>\n".
const DaemonPort = 5252

// MaxPayload bounds a single frame's JSON payload (16 MiB). File reads/writes
// larger than this must chunk (Phase 2); Phase 1 callers (exec output, config
// files) are comfortably under this.
const MaxPayload = 16 << 20

// Op is the 1-byte opcode identifying the operation a frame carries.
type Op uint8

const (
	OpHello     Op = 1 // handshake: HelloRequest -> HelloResponse
	OpExec      Op = 2 // ExecRequest -> ExecResponse
	OpReadFile  Op = 3 // ReadFileRequest -> ReadFileResponse
	OpWriteFile Op = 4 // WriteFileRequest -> WriteFileResponse
	OpDelete    Op = 5 // DeleteRequest -> DeleteResponse
	OpList      Op = 6 // ListRequest -> ListResponse
	OpStat      Op = 7 // StatRequest -> StatResponse
	OpError     Op = 255
)

func (o Op) String() string {
	switch o {
	case OpHello:
		return "hello"
	case OpExec:
		return "exec"
	case OpReadFile:
		return "readfile"
	case OpWriteFile:
		return "writefile"
	case OpDelete:
		return "delete"
	case OpList:
		return "list"
	case OpStat:
		return "stat"
	case OpError:
		return "error"
	default:
		return fmt.Sprintf("op(%d)", uint8(o))
	}
}

const headerLen = 2 + 1 + 4 // magic + opcode + length

// ErrBadMagic is returned when a frame does not begin with Magic.
var ErrBadMagic = errors.New("vsockwire: bad frame magic")

// ErrPayloadTooLarge is returned when a frame advertises a payload over
// MaxPayload.
var ErrPayloadTooLarge = errors.New("vsockwire: payload exceeds MaxPayload")

// WriteFrame marshals v to JSON and writes a single framed message to w.
func WriteFrame(w io.Writer, op Op, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("vsockwire: marshal %s: %w", op, err)
	}
	if len(payload) > MaxPayload {
		return ErrPayloadTooLarge
	}
	hdr := make([]byte, headerLen)
	hdr[0] = Magic[0]
	hdr[1] = Magic[1]
	hdr[2] = byte(op)
	binary.BigEndian.PutUint32(hdr[3:], uint32(len(payload)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	return nil
}

// ReadFrame reads a single framed message from r, returning its opcode and raw
// JSON payload. The caller unmarshals the payload into the struct matching the
// opcode.
func ReadFrame(r io.Reader) (Op, []byte, error) {
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return 0, nil, err
	}
	if hdr[0] != Magic[0] || hdr[1] != Magic[1] {
		return 0, nil, ErrBadMagic
	}
	op := Op(hdr[2])
	n := binary.BigEndian.Uint32(hdr[3:])
	if n > MaxPayload {
		return 0, nil, ErrPayloadTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return op, payload, nil
}

// ---- Request / Response payloads ----------------------------------------

// HelloRequest is sent by the host to negotiate the protocol version.
type HelloRequest struct {
	Version int `json:"version"`
}

// HelloResponse is the daemon's reply, echoing its version and identity.
type HelloResponse struct {
	Version int    `json:"version"`
	Daemon  string `json:"daemon"` // e.g. "pandastack-daemon"
}

// ExecRequest runs Cmd through /bin/sh -c in the guest.
type ExecRequest struct {
	Cmd string `json:"cmd"`
	// TimeoutMS, if >0, bounds the command's wall-clock runtime in the guest.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// ExecResponse mirrors guest.ExecResult.
type ExecResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
	// Err is set for daemon-side failures that are not a normal nonzero exit
	// (e.g. fork failure). Empty on success.
	Err string `json:"err,omitempty"`
}

// ReadFileRequest reads the whole file at Path.
type ReadFileRequest struct {
	Path string `json:"path"`
}

// ReadFileResponse carries the file bytes (base64 via json []byte) or an error.
type ReadFileResponse struct {
	Data []byte `json:"data,omitempty"`
	Err  string `json:"err,omitempty"`
}

// WriteFileRequest writes Data to Path, creating parent dirs.
type WriteFileRequest struct {
	Path string `json:"path"`
	Data []byte `json:"data,omitempty"`
	Mode uint32 `json:"mode,omitempty"` // 0 => default 0644
}

// WriteFileResponse reports success/failure.
type WriteFileResponse struct {
	Err string `json:"err,omitempty"`
}

// DeleteRequest removes Path recursively.
type DeleteRequest struct {
	Path string `json:"path"`
}

// DeleteResponse reports success/failure.
type DeleteResponse struct {
	Err string `json:"err,omitempty"`
}

// ListRequest lists the immediate children of directory Path.
type ListRequest struct {
	Path string `json:"path"`
}

// DirEntry mirrors guest.DirEntry.
type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	Mode  string `json:"mode"`
	Mtime int64  `json:"mtime"`
}

// ListResponse carries the directory entries or an error.
type ListResponse struct {
	Entries []DirEntry `json:"entries,omitempty"`
	Err     string     `json:"err,omitempty"`
}

// StatRequest stats a single Path.
type StatRequest struct {
	Path string `json:"path"`
}

// StatResponse carries the entry, or NotExist=true / Err for failures.
type StatResponse struct {
	Entry    *DirEntry `json:"entry,omitempty"`
	NotExist bool      `json:"not_exist,omitempty"`
	Err      string    `json:"err,omitempty"`
}

// ErrorPayload is sent with OpError for malformed/unknown requests.
type ErrorPayload struct {
	Err string `json:"err"`
}
