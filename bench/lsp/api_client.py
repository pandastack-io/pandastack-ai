"""PandaStack API client for LSP benchmark sandboxes."""

from __future__ import annotations

import asyncio
import json
import logging
from contextlib import asynccontextmanager
from typing import Any, AsyncIterator

import httpx
import websockets

LOG = logging.getLogger(__name__)


class PandaStackError(RuntimeError):
    pass


class LSPWebSocket:
    """Raw LSP-framed WebSocket client with response dispatch by JSON-RPC id."""

    def __init__(self, ws: Any):
        self.ws = ws
        self._pending: dict[int | str, asyncio.Future[dict[str, Any]]] = {}
        self.stderr: asyncio.Queue[str] = asyncio.Queue()
        self.notifications: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        self._reader_task = asyncio.create_task(self._reader())

    async def close(self) -> None:
        self._reader_task.cancel()
        try:
            await self._reader_task
        except asyncio.CancelledError:
            pass
        await self.ws.close()

    def _frame(self, payload: dict[str, Any]) -> bytes:
        body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        header = f"Content-Length: {len(body)}\r\n\r\n".encode("ascii")
        return header + body

    async def send(self, payload: dict[str, Any]) -> None:
        await self.ws.send(self._frame(payload))

    async def request(self, payload: dict[str, Any], timeout: float = 30.0) -> dict[str, Any]:
        req_id = payload.get("id")
        if req_id is None:
            raise ValueError("request payload must include id")
        loop = asyncio.get_running_loop()
        fut: asyncio.Future[dict[str, Any]] = loop.create_future()
        self._pending[req_id] = fut
        try:
            await self.send(payload)
            return await asyncio.wait_for(fut, timeout=timeout)
        finally:
            self._pending.pop(req_id, None)

    async def _reader(self) -> None:
        buffer = b""
        async for msg in self.ws:
            if isinstance(msg, str):
                try:
                    data = json.loads(msg)
                except json.JSONDecodeError:
                    await self.stderr.put(msg)
                    continue
                if data.get("stream") == "stderr":
                    await self.stderr.put(str(data.get("line", "")))
                else:
                    await self.notifications.put(data)
                continue
            buffer += msg
            while True:
                sep = buffer.find(b"\r\n\r\n")
                if sep < 0:
                    break
                header = buffer[:sep].decode("ascii", errors="replace")
                length = None
                for line in header.split("\r\n"):
                    if line.lower().startswith("content-length:"):
                        length = int(line.split(":", 1)[1].strip())
                        break
                if length is None:
                    raise PandaStackError(f"LSP frame missing Content-Length: {header!r}")
                start = sep + 4
                end = start + length
                if len(buffer) < end:
                    break
                body = buffer[start:end]
                buffer = buffer[end:]
                await self._handle_payload(json.loads(body.decode("utf-8")))

    async def _handle_payload(self, payload: dict[str, Any]) -> None:
        msg_id = payload.get("id")
        if msg_id in self._pending:
            fut = self._pending[msg_id]
            if not fut.done():
                fut.set_result(payload)
        else:
            await self.notifications.put(payload)


class PandaStackClient:
    def __init__(self, base_url: str, token: str, timeout: float = 60.0):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.headers = {"Authorization": f"Bearer {token}"}
        self.http = httpx.AsyncClient(base_url=self.base_url, headers=self.headers, timeout=timeout)

    async def close(self) -> None:
        await self.http.aclose()

    async def _raise_for_status(self, response: httpx.Response) -> None:
        try:
            response.raise_for_status()
        except httpx.HTTPStatusError as exc:
            raise PandaStackError(f"{response.request.method} {response.request.url} failed: {response.status_code} {response.text}") from exc

    async def list_sandboxes(self) -> Any:
        resp = await self.http.get("/v1/sandboxes")
        await self._raise_for_status(resp)
        return resp.json()

    async def create_sandbox(self, template: str = "code-interpreter", memory_mb: int = 1024, vcpus: int = 2) -> dict[str, Any]:
        resp = await self.http.post("/v1/sandboxes", json={"template": template, "memory_mb": memory_mb, "vcpus": vcpus})
        await self._raise_for_status(resp)
        return resp.json()

    async def exec(self, sandbox_id: str, cmd: str, timeout_seconds: int = 60) -> dict[str, Any]:
        resp = await self.http.post(f"/v1/sandboxes/{sandbox_id}/exec", json={"cmd": cmd, "timeout_seconds": timeout_seconds}, timeout=timeout_seconds + 10)
        await self._raise_for_status(resp)
        data = resp.json()
        if "exit" not in data and "exit_code" in data:
            data["exit"] = data["exit_code"]
        return data

    async def upload_file(self, sandbox_id: str, path: str, content: str | bytes) -> None:
        data = content.encode("utf-8") if isinstance(content, str) else content
        resp = await self.http.put(f"/v1/sandboxes/{sandbox_id}/fs", params={"path": path}, content=data)
        await self._raise_for_status(resp)

    async def read_file(self, sandbox_id: str, path: str) -> bytes:
        resp = await self.http.get(f"/v1/sandboxes/{sandbox_id}/fs", params={"path": path})
        await self._raise_for_status(resp)
        return resp.content

    async def delete_sandbox(self, sandbox_id: str) -> None:
        resp = await self.http.delete(f"/v1/sandboxes/{sandbox_id}")
        if resp.status_code not in (200, 202, 204, 404):
            await self._raise_for_status(resp)

    @asynccontextmanager
    async def lsp_ws(self, sandbox_id: str) -> AsyncIterator[LSPWebSocket]:
        ws_url = self.base_url.replace("https://", "wss://").replace("http://", "ws://") + f"/v1/sandboxes/{sandbox_id}/lsp/python"
        kwargs = {"extra_headers": self.headers, "subprotocols": ["lsp"]}
        try:
            ws = await websockets.connect(ws_url, **kwargs)
        except TypeError:
            ws = await websockets.connect(ws_url, additional_headers=self.headers, subprotocols=["lsp"])
        client = LSPWebSocket(ws)
        try:
            yield client
        finally:
            await client.close()
