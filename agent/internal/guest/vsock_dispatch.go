// SPDX-License-Identifier: Apache-2.0
package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/pandastack/agent/internal/guest/vsockwire"
)

// This file holds the vsock-first dispatchers for the six operations that the
// Phase-1 fast-path covers: Exec, ReadFile, WriteFile, DeletePath, ListDir,
// Stat. Each tries the in-guest pandastack-daemon over vsock first (when
// EnableVsock has been called) and transparently falls back to the SSH
// implementation (the *SSH methods in ssh.go) on any transport failure.
//
// PTY (OpenPTY), streaming exec (ExecStream), and OpenProc stay SSH-only in
// Phase 1; they are not wrapped here.

// opDeadline bounds a single vsock op end-to-end. Generous because Exec can run
// arbitrarily long user commands; the daemon also honours ExecRequest.TimeoutMS
// for command-level bounding.
const opDeadline = 0 // 0 => no host-side frame deadline (rely on ctx + Exec timeout)

func (c *Client) Exec(ctx context.Context, cmd string) (*ExecResult, error) {
	if c.vsockEnabled() {
		res, err := c.execVsock(ctx, cmd)
		if err == nil {
			return res, nil
		}
		if !isVsockUnavailable(err) {
			return nil, err
		}
		// transport failed → fall through to SSH
	}
	return c.execSSH(ctx, cmd)
}

func (c *Client) execVsock(ctx context.Context, cmd string) (*ExecResult, error) {
	respOp, payload, err := c.vsockRoundTrip(ctx, vsockwire.OpExec,
		vsockwire.ExecRequest{Cmd: cmd}, opDeadline)
	if err != nil {
		return nil, err
	}
	if respOp == vsockwire.OpError {
		return nil, decodeDaemonError(payload)
	}
	var resp vsockwire.ExecResponse
	if err := jsonUnmarshal(payload, &resp); err != nil {
		return nil, errVsockUnavailable{err}
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	return &ExecResult{Stdout: resp.Stdout, Stderr: resp.Stderr, ExitCode: resp.ExitCode}, nil
}

func (c *Client) ReadFile(ctx context.Context, p string) ([]byte, error) {
	if err := validatePath(p); err != nil {
		return nil, err
	}
	if c.vsockEnabled() {
		data, err := c.readFileVsock(ctx, p)
		if err == nil {
			return data, nil
		}
		if !isVsockUnavailable(err) {
			return nil, err
		}
	}
	return c.readFileSSH(ctx, p)
}

func (c *Client) readFileVsock(ctx context.Context, p string) ([]byte, error) {
	respOp, payload, err := c.vsockRoundTrip(ctx, vsockwire.OpReadFile,
		vsockwire.ReadFileRequest{Path: p}, opDeadline)
	if err != nil {
		return nil, err
	}
	if respOp == vsockwire.OpError {
		return nil, decodeDaemonError(payload)
	}
	var resp vsockwire.ReadFileResponse
	if err := jsonUnmarshal(payload, &resp); err != nil {
		return nil, errVsockUnavailable{err}
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	return resp.Data, nil
}

func (c *Client) WriteFile(ctx context.Context, p string, data []byte) error {
	if err := validatePath(p); err != nil {
		return err
	}
	if c.vsockEnabled() {
		err := c.writeFileVsock(ctx, p, data)
		if err == nil {
			return nil
		}
		if !isVsockUnavailable(err) {
			return err
		}
	}
	return c.writeFileSSH(ctx, p, data)
}

func (c *Client) writeFileVsock(ctx context.Context, p string, data []byte) error {
	respOp, payload, err := c.vsockRoundTrip(ctx, vsockwire.OpWriteFile,
		vsockwire.WriteFileRequest{Path: p, Data: data}, opDeadline)
	if err != nil {
		return err
	}
	if respOp == vsockwire.OpError {
		return decodeDaemonError(payload)
	}
	var resp vsockwire.WriteFileResponse
	if err := jsonUnmarshal(payload, &resp); err != nil {
		return errVsockUnavailable{err}
	}
	if resp.Err != "" {
		return errors.New(resp.Err)
	}
	return nil
}

func (c *Client) DeletePath(ctx context.Context, p string) error {
	if err := validatePath(p); err != nil {
		return err
	}
	if c.vsockEnabled() {
		err := c.deletePathVsock(ctx, p)
		if err == nil {
			return nil
		}
		if !isVsockUnavailable(err) {
			return err
		}
	}
	return c.deletePathSSH(ctx, p)
}

func (c *Client) deletePathVsock(ctx context.Context, p string) error {
	respOp, payload, err := c.vsockRoundTrip(ctx, vsockwire.OpDelete,
		vsockwire.DeleteRequest{Path: p}, opDeadline)
	if err != nil {
		return err
	}
	if respOp == vsockwire.OpError {
		return decodeDaemonError(payload)
	}
	var resp vsockwire.DeleteResponse
	if err := jsonUnmarshal(payload, &resp); err != nil {
		return errVsockUnavailable{err}
	}
	if resp.Err != "" {
		return errors.New(resp.Err)
	}
	return nil
}

func (c *Client) ListDir(ctx context.Context, p string) ([]DirEntry, error) {
	if err := validatePath(p); err != nil {
		return nil, err
	}
	if c.vsockEnabled() {
		entries, err := c.listDirVsock(ctx, p)
		if err == nil {
			return entries, nil
		}
		if !isVsockUnavailable(err) {
			return nil, err
		}
	}
	return c.listDirSSH(ctx, p)
}

func (c *Client) listDirVsock(ctx context.Context, p string) ([]DirEntry, error) {
	respOp, payload, err := c.vsockRoundTrip(ctx, vsockwire.OpList,
		vsockwire.ListRequest{Path: p}, opDeadline)
	if err != nil {
		return nil, err
	}
	if respOp == vsockwire.OpError {
		return nil, decodeDaemonError(payload)
	}
	var resp vsockwire.ListResponse
	if err := jsonUnmarshal(payload, &resp); err != nil {
		return nil, errVsockUnavailable{err}
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	out := make([]DirEntry, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		out = append(out, DirEntry{
			Name: e.Name, IsDir: e.IsDir, Size: e.Size, Mode: e.Mode, Mtime: e.Mtime,
		})
	}
	return out, nil
}

func (c *Client) Stat(ctx context.Context, p string) (*DirEntry, error) {
	if err := validatePath(p); err != nil {
		return nil, err
	}
	if c.vsockEnabled() {
		de, err := c.statVsock(ctx, p)
		if err == nil {
			return de, nil
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if !isVsockUnavailable(err) {
			return nil, err
		}
	}
	return c.statSSH(ctx, p)
}

func (c *Client) statVsock(ctx context.Context, p string) (*DirEntry, error) {
	respOp, payload, err := c.vsockRoundTrip(ctx, vsockwire.OpStat,
		vsockwire.StatRequest{Path: p}, opDeadline)
	if err != nil {
		return nil, err
	}
	if respOp == vsockwire.OpError {
		return nil, decodeDaemonError(payload)
	}
	var resp vsockwire.StatResponse
	if err := jsonUnmarshal(payload, &resp); err != nil {
		return nil, errVsockUnavailable{err}
	}
	if resp.NotExist {
		return nil, os.ErrNotExist
	}
	if resp.Err != "" {
		return nil, errors.New(resp.Err)
	}
	if resp.Entry == nil {
		return nil, os.ErrNotExist
	}
	return &DirEntry{
		Name:  resp.Entry.Name,
		IsDir: resp.Entry.IsDir,
		Size:  resp.Entry.Size,
		Mode:  resp.Entry.Mode,
		Mtime: resp.Entry.Mtime,
	}, nil
}

// decodeDaemonError turns an OpError payload into a normal error. The request
// reached the daemon but was rejected (bad opcode/payload/path), so we do NOT
// fall back to SSH — SSH would reject it the same way.
func decodeDaemonError(payload []byte) error {
	var ep vsockwire.ErrorPayload
	if err := jsonUnmarshal(payload, &ep); err != nil {
		return fmt.Errorf("daemon error (undecodable): %w", err)
	}
	return errors.New(ep.Err)
}

func jsonUnmarshal(payload []byte, v any) error {
	return json.Unmarshal(payload, v)
}
