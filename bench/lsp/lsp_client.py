"""Minimal Python LSP client over PandaStack's raw-framed WebSocket.

Responses are returned in compact, token-efficient form:
  * locations  → "<path>:<line>:<col>" strings (line/col are 1-based for humans)
  * symbols    → {"name", "kind", "loc"} where loc is the same string form
This is ~70% smaller than the raw LSP wire format and dramatically reduces
token cost when these payloads are fed to an LLM tool-calling loop.
"""

from __future__ import annotations

import itertools
from pathlib import PurePosixPath
from typing import Any

from api_client import LSPWebSocket


# LSP SymbolKind enum (subset we care about) → short string
_KIND = {
    1: "file", 2: "module", 3: "namespace", 4: "package",
    5: "class", 6: "method", 7: "property", 8: "field",
    9: "constructor", 10: "enum", 11: "interface", 12: "function",
    13: "variable", 14: "constant", 22: "struct", 23: "event", 24: "operator",
}


def _strip_uri(uri: str) -> str:
    if uri.startswith("file://"):
        return uri[len("file://"):]
    return uri


def _loc_str(uri: str, rng: dict[str, Any]) -> str:
    """Render an LSP location as '<path>:<line>:<col>' (1-based)."""
    path = _strip_uri(uri)
    start = (rng or {}).get("start") or {}
    line = int(start.get("line", 0)) + 1
    col = int(start.get("character", 0)) + 1
    return f"{path}:{line}:{col}"


class LSPClient:
    def __init__(self, transport: LSPWebSocket):
        self.transport = transport
        self._ids = itertools.count(1)

    def _next_id(self) -> int:
        return next(self._ids)

    def _uri(self, path: str) -> str:
        return path if path.startswith("file://") else "file://" + path

    async def initialize(self, rootUri: str) -> dict[str, Any]:
        root = rootUri if rootUri.startswith("file://") else "file://" + rootUri.rstrip("/")
        resp = await self._request("initialize", {
            "processId": None,
            "rootUri": root,
            "capabilities": {},
            "workspaceFolders": [{"uri": root, "name": PurePosixPath(root.replace("file://", "")).name or "workspace"}],
        }, timeout=120.0)
        await self._notify("initialized", {})
        return resp

    async def did_open(self, path: str, text: str) -> None:
        await self._notify("textDocument/didOpen", {
            "textDocument": {
                "uri": self._uri(path),
                "languageId": "python",
                "version": 1,
                "text": text,
            }
        })

    async def definition(self, path: str, line: int, character: int) -> list[str]:
        result = await self._request("textDocument/definition", self._position(path, line, character))
        return self._locations(result)

    async def definition_raw(self, path: str, line: int, character: int) -> list[dict[str, Any]]:
        """Same as definition() but keeps raw uri+range. Used by combined goto_def."""
        result = await self._request("textDocument/definition", self._position(path, line, character))
        return self._locations_raw(result)

    async def references(self, path: str, line: int, character: int) -> list[str]:
        params = self._position(path, line, character)
        params["context"] = {"includeDeclaration": True}
        return self._locations(await self._request("textDocument/references", params))

    async def hover(self, path: str, line: int, character: int) -> str:
        result = await self._request("textDocument/hover", self._position(path, line, character))
        contents = (result or {}).get("contents", "") if isinstance(result, dict) else ""
        if isinstance(contents, str):
            return contents
        if isinstance(contents, dict):
            return str(contents.get("value", contents))
        if isinstance(contents, list):
            parts = []
            for item in contents:
                parts.append(item.get("value", "") if isinstance(item, dict) else str(item))
            return "\n".join(parts)
        return str(contents)

    async def document_symbol(self, path: str) -> list[dict[str, Any]]:
        result = await self._request("textDocument/documentSymbol", {"textDocument": {"uri": self._uri(path)}})
        return self._symbols(result, default_uri=self._uri(path))

    async def workspace_symbol(self, query: str) -> list[dict[str, Any]]:
        try:
            result = await self._request("workspace/symbol", {"query": query})
        except RuntimeError as exc:
            # If pylsp-rope is not installed pylsp returns -32601 Method Not Found.
            # The template now bakes in pylsp-rope, but keep the guard for older sandboxes.
            if "-32601" in str(exc) or "Method Not Found" in str(exc):
                return []
            raise
        return self._symbols(result)

    async def _request(self, method: str, params: dict[str, Any], timeout: float = 30.0) -> Any:
        payload = {"jsonrpc": "2.0", "id": self._next_id(), "method": method, "params": params}
        resp = await self.transport.request(payload, timeout=timeout)
        if "error" in resp:
            raise RuntimeError(f"LSP {method} failed: {resp['error']}")
        return resp.get("result")

    async def _notify(self, method: str, params: dict[str, Any]) -> None:
        await self.transport.send({"jsonrpc": "2.0", "method": method, "params": params})

    def _position(self, path: str, line: int, character: int) -> dict[str, Any]:
        return {"textDocument": {"uri": self._uri(path)}, "position": {"line": line, "character": character}}

    def _locations(self, result: Any) -> list[str]:
        out: list[str] = []
        for item in self._locations_raw(result):
            out.append(_loc_str(item["uri"], item["range"]))
        return out

    def _locations_raw(self, result: Any) -> list[dict[str, Any]]:
        if not result:
            return []
        items = result if isinstance(result, list) else [result]
        out = []
        for item in items:
            target = item.get("targetUri") or item.get("uri")
            rng = item.get("targetRange") or item.get("range")
            if target and rng:
                out.append({"uri": target, "range": rng})
        return out

    def _symbols(self, result: Any, default_uri: str | None = None) -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = []
        def add(sym: dict[str, Any]) -> None:
            location = sym.get("location") or {}
            uri = location.get("uri") or default_uri or ""
            rng = sym.get("range") or location.get("range") or {}
            kind_num = sym.get("kind")
            out.append({
                "name": sym.get("name", ""),
                "kind": _KIND.get(kind_num, str(kind_num) if kind_num else ""),
                "loc": _loc_str(uri, rng) if uri else "",
            })
            for child in sym.get("children", []) or []:
                add(child)
        for sym in result or []:
            if isinstance(sym, dict):
                add(sym)
        return out
