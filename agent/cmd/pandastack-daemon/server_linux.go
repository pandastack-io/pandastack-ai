// SPDX-License-Identifier: Apache-2.0
//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/pandastack/agent/internal/guest/vsockwire"
)

const defaultPort = vsockwire.DaemonPort

// run binds the AF_VSOCK listener and serves connections forever. It is
// resilient: transient accept errors are logged and retried so a snapshot
// restore (or a peer reset) never tears the daemon down.
func run(port uint32, verbose bool) error {
	ln, err := vsockListen(port)
	if err != nil {
		return fmt.Errorf("listen vsock:%d: %w", port, err)
	}
	defer ln.Close()
	log.Printf("listening on vsock port %d", port)

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			// Transient (EINTR/EAGAIN handled in Accept); back off briefly.
			log.Printf("accept: %v", err)
			time.Sleep(5 * time.Millisecond)
			continue
		}
		// One request per connection, served inline. Handlers are short and
		// the host opens a fresh connection per op, so we do not need a
		// goroutine-per-conn pool; serving inline keeps ordering simple and
		// avoids unbounded fan-out. If throughput ever needs it, wrap in `go`.
		serve(conn, verbose)
	}
}

// serve reads one request frame, dispatches it, and writes one response frame.
func serve(conn net.Conn, verbose bool) {
	defer conn.Close()
	// Bound a single request so a stuck peer cannot pin a connection forever.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Minute))

	op, payload, err := vsockwire.ReadFrame(conn)
	if err != nil {
		if verbose {
			log.Printf("read frame: %v", err)
		}
		return
	}
	if verbose {
		log.Printf("op=%s payloadLen=%d", op, len(payload))
	}

	respOp, resp := dispatch(op, payload)
	if err := vsockwire.WriteFrame(conn, respOp, resp); err != nil {
		if verbose {
			log.Printf("write frame: %v", err)
		}
	}
}

// dispatch decodes the request payload for op and returns the response opcode
// and value to send back.
func dispatch(op vsockwire.Op, payload []byte) (vsockwire.Op, any) {
	switch op {
	case vsockwire.OpHello:
		return vsockwire.OpHello, vsockwire.HelloResponse{
			Version: vsockwire.ProtocolVersion,
			Daemon:  "pandastack-daemon",
		}
	case vsockwire.OpExec:
		var req vsockwire.ExecRequest
		if err := unmarshal(payload, &req); err != nil {
			return errResp(err)
		}
		return vsockwire.OpExec, handleExec(req)
	case vsockwire.OpReadFile:
		var req vsockwire.ReadFileRequest
		if err := unmarshal(payload, &req); err != nil {
			return errResp(err)
		}
		return vsockwire.OpReadFile, handleReadFile(req)
	case vsockwire.OpWriteFile:
		var req vsockwire.WriteFileRequest
		if err := unmarshal(payload, &req); err != nil {
			return errResp(err)
		}
		return vsockwire.OpWriteFile, handleWriteFile(req)
	case vsockwire.OpDelete:
		var req vsockwire.DeleteRequest
		if err := unmarshal(payload, &req); err != nil {
			return errResp(err)
		}
		return vsockwire.OpDelete, handleDelete(req)
	case vsockwire.OpList:
		var req vsockwire.ListRequest
		if err := unmarshal(payload, &req); err != nil {
			return errResp(err)
		}
		return vsockwire.OpList, handleList(req)
	case vsockwire.OpStat:
		var req vsockwire.StatRequest
		if err := unmarshal(payload, &req); err != nil {
			return errResp(err)
		}
		return vsockwire.OpStat, handleStat(req)
	default:
		return vsockwire.OpError, vsockwire.ErrorPayload{
			Err: fmt.Sprintf("unknown opcode %d", uint8(op)),
		}
	}
}

func errResp(err error) (vsockwire.Op, any) {
	return vsockwire.OpError, vsockwire.ErrorPayload{Err: err.Error()}
}

// ---- handlers -----------------------------------------------------------
//
// Each handler mirrors the corresponding guest/ssh.go method so the vsock
// fast-path is behaviourally identical to the SSH transport. FS ops shell out
// through /bin/sh -c with the same commands (cat/mkdir+cat/rm/find) so output
// parsing on the host matches exactly.

// shexec runs cmd through /bin/sh -c, capturing stdout/stderr/exit, mirroring
// guest.Client.Exec. A nonzero exit is NOT an error here — it is reported via
// ExitCode, exactly like the SSH path.
func shexec(ctx context.Context, cmd string, stdin []byte) (stdout, stderr string, exit int, runErr error) {
	c := exec.CommandContext(ctx, "/bin/sh", "-c", cmd)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	var so, se bytes.Buffer
	c.Stdout = &so
	c.Stderr = &se
	err := c.Run()
	exit = 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			// Could not start the process at all (e.g. /bin/sh missing).
			return so.String(), se.String(), -1, err
		}
	}
	return so.String(), se.String(), exit, nil
}

func handleExec(req vsockwire.ExecRequest) vsockwire.ExecResponse {
	ctx := context.Background()
	if req.TimeoutMS > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
		defer cancel()
	}
	stdout, stderr, exit, err := shexec(ctx, req.Cmd, nil)
	resp := vsockwire.ExecResponse{Stdout: stdout, Stderr: stderr, ExitCode: exit}
	if err != nil {
		resp.Err = err.Error()
	}
	return resp
}

func handleReadFile(req vsockwire.ReadFileRequest) vsockwire.ReadFileResponse {
	if err := validatePath(req.Path); err != nil {
		return vsockwire.ReadFileResponse{Err: err.Error()}
	}
	cmd := fmt.Sprintf("cat -- %s", shellQuote(req.Path))
	stdout, stderr, exit, err := shexec(context.Background(), cmd, nil)
	if err != nil {
		return vsockwire.ReadFileResponse{Err: err.Error()}
	}
	if exit != 0 {
		return vsockwire.ReadFileResponse{
			Err: fmt.Sprintf("read %s: exit %d: %s", req.Path, exit, strings.TrimSpace(stderr)),
		}
	}
	return vsockwire.ReadFileResponse{Data: []byte(stdout)}
}

func handleWriteFile(req vsockwire.WriteFileRequest) vsockwire.WriteFileResponse {
	if err := validatePath(req.Path); err != nil {
		return vsockwire.WriteFileResponse{Err: err.Error()}
	}
	parent := path.Dir(req.Path)
	cmd := fmt.Sprintf("mkdir -p -- %s && cat > %s", shellQuote(parent), shellQuote(req.Path))
	_, stderr, exit, err := shexec(context.Background(), cmd, req.Data)
	if err != nil {
		return vsockwire.WriteFileResponse{Err: err.Error()}
	}
	if exit != 0 {
		return vsockwire.WriteFileResponse{
			Err: fmt.Sprintf("write %s: exit %d (%s)", req.Path, exit, strings.TrimSpace(stderr)),
		}
	}
	// Optional explicit mode (SSH path leaves default umask; we only chmod when asked).
	if req.Mode != 0 {
		chmod := fmt.Sprintf("chmod %o -- %s", req.Mode&0o7777, shellQuote(req.Path))
		_, se, ex, cerr := shexec(context.Background(), chmod, nil)
		if cerr != nil {
			return vsockwire.WriteFileResponse{Err: cerr.Error()}
		}
		if ex != 0 {
			return vsockwire.WriteFileResponse{Err: fmt.Sprintf("chmod %s: %s", req.Path, strings.TrimSpace(se))}
		}
	}
	return vsockwire.WriteFileResponse{}
}

func handleDelete(req vsockwire.DeleteRequest) vsockwire.DeleteResponse {
	if err := validatePath(req.Path); err != nil {
		return vsockwire.DeleteResponse{Err: err.Error()}
	}
	cmd := fmt.Sprintf("rm -rf -- %s", shellQuote(req.Path))
	_, stderr, exit, err := shexec(context.Background(), cmd, nil)
	if err != nil {
		return vsockwire.DeleteResponse{Err: err.Error()}
	}
	if exit != 0 {
		return vsockwire.DeleteResponse{Err: fmt.Sprintf("delete %s: %s", req.Path, strings.TrimSpace(stderr))}
	}
	return vsockwire.DeleteResponse{}
}

func handleList(req vsockwire.ListRequest) vsockwire.ListResponse {
	if err := validatePath(req.Path); err != nil {
		return vsockwire.ListResponse{Err: err.Error()}
	}
	cmd := fmt.Sprintf(
		`find %s -mindepth 1 -maxdepth 1 -printf '%%y|%%s|%%T@|%%f\0'`,
		shellQuote(req.Path),
	)
	stdout, stderr, exit, err := shexec(context.Background(), cmd, nil)
	if err != nil {
		return vsockwire.ListResponse{Err: err.Error()}
	}
	if exit != 0 {
		return vsockwire.ListResponse{Err: fmt.Sprintf("listdir %s: %s", req.Path, strings.TrimSpace(stderr))}
	}
	var entries []vsockwire.DirEntry
	for _, rec := range strings.Split(stdout, "\x00") {
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "|", 4)
		if len(parts) != 4 {
			continue
		}
		var size int64
		fmt.Sscanf(parts[1], "%d", &size)
		var mtimeF float64
		fmt.Sscanf(parts[2], "%f", &mtimeF)
		entries = append(entries, vsockwire.DirEntry{
			Name:  parts[3],
			IsDir: parts[0] == "d",
			Size:  size,
			Mode:  parts[0],
			Mtime: int64(mtimeF),
		})
	}
	return vsockwire.ListResponse{Entries: entries}
}

func handleStat(req vsockwire.StatRequest) vsockwire.StatResponse {
	if err := validatePath(req.Path); err != nil {
		return vsockwire.StatResponse{Err: err.Error()}
	}
	cmd := fmt.Sprintf(`find %s -maxdepth 0 -printf '%%y|%%s|%%T@|%%f\0'`, shellQuote(req.Path))
	stdout, _, exit, err := shexec(context.Background(), cmd, nil)
	if err != nil {
		return vsockwire.StatResponse{Err: err.Error()}
	}
	if exit != 0 || strings.TrimSpace(stdout) == "" {
		return vsockwire.StatResponse{NotExist: true}
	}
	rec := strings.TrimRight(stdout, "\x00")
	parts := strings.SplitN(rec, "|", 4)
	if len(parts) != 4 {
		return vsockwire.StatResponse{Err: "bad stat output"}
	}
	var size int64
	fmt.Sscanf(parts[1], "%d", &size)
	var mtimeF float64
	fmt.Sscanf(parts[2], "%f", &mtimeF)
	return vsockwire.StatResponse{Entry: &vsockwire.DirEntry{
		Name:  path.Base(req.Path),
		IsDir: parts[0] == "d",
		Size:  size,
		Mode:  parts[0],
		Mtime: int64(mtimeF),
	}}
}

// ---- helpers (mirrors guest/ssh.go) -------------------------------------

func validatePath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("path must be absolute, got %q", p)
	}
	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func unmarshal(payload []byte, v any) error {
	return json.Unmarshal(payload, v)
}
