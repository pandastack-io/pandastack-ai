"""OpenAI tool-calling bug-fixing agent for PandaStack sandboxes."""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field
from typing import Any

from api_client import PandaStackClient, PandaStackError
from lsp_client import LSPClient


@dataclass
class AgentResult:
    passed: bool = False
    steps: int = 0
    input_tokens: int = 0
    output_tokens: int = 0
    tool_calls: list[dict[str, Any]] = field(default_factory=list)
    error: str = ""

    @property
    def total_tokens(self) -> int:
        return self.input_tokens + self.output_tokens

    @property
    def hallucinated_count(self) -> int:
        count = 0
        for call in self.tool_calls:
            name = call.get("name")
            if name in {"lsp_find_symbol", "lsp_references", "lsp_goto_def", "grep"} and call.get("empty"):
                count += 1
            if name == "read_file" and call.get("missing"):
                count += 1
        return count


class BugFixAgent:
    def __init__(
        self,
        api: PandaStackClient,
        sandbox_id: str,
        task: dict[str, Any],
        model: str = "gpt-4o-mini",
        max_steps: int = 25,
        lsp: LSPClient | None = None,
    ):
        self.api = api
        self.sandbox_id = sandbox_id
        self.task = task
        self.model = model
        self.max_steps = max_steps
        self.lsp = lsp
        self.result = AgentResult()

    async def run(self) -> AgentResult:
        if not os.environ.get("OPENAI_API_KEY"):
            raise RuntimeError("OPENAI_API_KEY is not set. Use --dry-run to smoke-test without OpenAI tokens.")
        from openai import OpenAI

        client = OpenAI()
        tools = self._tool_specs(include_lsp=self.lsp is not None)
        sys_lsp_hint = ""
        if self.lsp is not None:
            sys_lsp_hint = (
                " You have Python LSP tools available. Use `lsp_find_symbol(name)` FIRST when the failing "
                "test mentions a symbol whose definition you can't see in front of you — it returns "
                "`{name, kind, loc, preview}` for every place the symbol is defined in the workspace. "
                "Use `lsp_goto_def(file, line, character)` to jump from a use-site to a def AND read the "
                "surrounding code in one call (don't separately read_file the def site). For trivial "
                "single-file edits, prefer read_file + write_file — LSP is for navigation."
            )
        messages: list[dict[str, Any]] = [
            {"role": "system", "content": f"You are a Python bug-fixing agent. You have {len(tools)} tools. Use them to investigate the failing test and produce a minimal patch. When done, call `submit()` to run the test.{sys_lsp_hint}"},
            {"role": "user", "content": self.task["prompt"]},
        ]
        for _ in range(self.max_steps):
            self.result.steps += 1
            resp = client.chat.completions.create(model=self.model, messages=messages, tools=tools, tool_choice="auto")
            usage = getattr(resp, "usage", None)
            if usage:
                self.result.input_tokens += int(getattr(usage, "prompt_tokens", 0) or 0)
                self.result.output_tokens += int(getattr(usage, "completion_tokens", 0) or 0)
            msg = resp.choices[0].message
            messages.append(msg.model_dump(exclude_none=True))
            if not msg.tool_calls:
                continue
            for tool_call in msg.tool_calls:
                args = json.loads(tool_call.function.arguments or "{}")
                output = await self.execute_tool(tool_call.function.name, args)
                messages.append({"role": "tool", "tool_call_id": tool_call.id, "content": output})
                if tool_call.function.name == "submit" and self.result.passed:
                    return self.result
        if not self.result.passed:
            self.result.error = self.result.error or "max steps reached without passing submit"
        return self.result

    async def run_stub(self, apply_fix: bool = True) -> AgentResult:
        paths = sorted(self.task["files"].keys())
        for path in paths:
            await self.execute_tool("read_file", {"path": path})
        if apply_fix:
            for path, content in self.task.get("dry_run_fix", {}).items():
                await self.execute_tool("write_file", {"path": path, "content": content})
        await self.execute_tool("submit", {})
        self.result.steps = len(self.result.tool_calls)
        return self.result

    async def execute_tool(self, name: str, args: dict[str, Any]) -> str:
        record: dict[str, Any] = {"name": name, "args": args}
        try:
            if name == "read_file":
                data = await self.api.read_file(self.sandbox_id, args["path"])
                text = data.decode("utf-8", errors="replace")
                record["bytes"] = len(data)
                return self._record(record, text)
            if name == "write_file":
                await self.api.upload_file(self.sandbox_id, args["path"], args["content"])
                record["bytes"] = len(args["content"].encode("utf-8"))
                return self._record(record, "ok")
            if name == "list_dir":
                path_literal = json.dumps(args["path"])
                cmd = f"python - <<'PY'\nimport os, json\np={path_literal}\nprint(json.dumps(sorted(os.listdir(p))))\nPY"
                res = await self.api.exec(self.sandbox_id, cmd, int(args.get("timeout", 30)))
                record["exit"] = res.get("exit")
                return self._record(record, self._format_exec(res))
            if name == "exec":
                res = await self.api.exec(self.sandbox_id, args["cmd"], int(args.get("timeout", 30)))
                record["exit"] = res.get("exit")
                return self._record(record, self._format_exec(res))
            if name == "submit":
                res = await self.api.exec(self.sandbox_id, self.task["failing_test"], 60)
                passed = int(res.get("exit", 1)) == 0
                self.result.passed = passed
                record["exit"] = res.get("exit")
                record["passed"] = passed
                return self._record(record, ("PASS\n" if passed else "FAIL\n") + self._format_exec(res))
            if name.startswith("lsp_"):
                if not self.lsp:
                    raise RuntimeError("LSP tool called but LSP client is disabled")
                value = await self._execute_lsp(name, args)
                if value == [] or value == "":
                    record["empty"] = True
                return self._record(record, json.dumps(value, indent=2))
            raise ValueError(f"unknown tool {name}")
        except PandaStackError as exc:
            if name == "read_file":
                record["missing"] = True
            return self._record(record, f"ERROR: {exc}")
        except Exception as exc:  # keep agent loop alive and measurable
            if name == "read_file":
                record["missing"] = True
            return self._record(record, f"ERROR: {type(exc).__name__}: {exc}")

    async def _execute_lsp(self, name: str, args: dict[str, Any]) -> Any:
        assert self.lsp is not None
        if name == "lsp_goto_def":
            # Combined: definition lookup + the actual code at the def site.
            # Eliminates the def -> read_file round-trip the agent used to do.
            locs = await self.lsp.definition_raw(args["file"], int(args["line"]), int(args["character"]))
            if not locs:
                return []
            context = int(args.get("context_lines", 12))
            results: list[dict[str, Any]] = []
            for loc in locs[:3]:  # cap to top-3 def candidates
                from lsp_client import _strip_uri, _loc_str
                path = _strip_uri(loc["uri"])
                start_line = int((loc["range"].get("start") or {}).get("line", 0))
                try:
                    data = await self.api.read_file(self.sandbox_id, path)
                    text = data.decode("utf-8", errors="replace")
                    lines = text.split("\n")
                    lo = max(0, start_line - 2)
                    hi = min(len(lines), start_line + context)
                    snippet_lines = []
                    for i in range(lo, hi):
                        marker = ">" if i == start_line else " "
                        snippet_lines.append(f"{marker}{i+1:4d} {lines[i]}")
                    snippet = "\n".join(snippet_lines)
                except Exception as exc:
                    snippet = f"(failed to read {path}: {exc})"
                results.append({"loc": _loc_str(loc["uri"], loc["range"]), "code": snippet})
            return results
        if name == "lsp_references":
            return await self.lsp.references(args["file"], int(args["line"]), int(args["character"]))
        if name == "lsp_outline":
            return await self.lsp.document_symbol(args["file"])
        if name == "lsp_find_symbol":
            # pylsp's workspace/symbol is unimplemented even with pylsp-rope.
            # Fall back to a structural grep so this tool always returns something
            # useful: `def <q>`, `class <q>`, and bare `<q> =` (top-level assignment).
            results = await self.lsp.workspace_symbol(args["query"])
            if results:
                return results
            q = args["query"].replace("'", "").replace('"', "")
            # Escape only regex metas we care about (the query is almost always a Python identifier).
            cmd = (
                f"grep -rnE '^[[:space:]]*(def|class)[[:space:]]+{q}\\b|^[[:space:]]*{q}[[:space:]]*=' "
                f"--include='*.py' /workspace 2>/dev/null | head -25"
            )
            res = await self.api.exec(self.sandbox_id, cmd, 15)
            out: list[dict[str, Any]] = []
            for line in (res.get("stdout") or "").splitlines():
                # grep -n output: "<path>:<line>:<text>"
                parts = line.split(":", 2)
                if len(parts) < 3:
                    continue
                path, lineno, text = parts[0], parts[1], parts[2].strip()
                kind = "function" if text.startswith("def ") else ("class" if text.startswith("class ") else "variable")
                out.append({"name": args["query"], "kind": kind, "loc": f"{path}:{lineno}:1", "preview": text[:120]})
            return out
        raise ValueError(f"unknown LSP tool {name}")

    def _record(self, record: dict[str, Any], output: str) -> str:
        record["empty"] = record.get("empty", output.strip() == "" or output.strip() == "[]")
        record["output_preview"] = output[:500]
        self.result.tool_calls.append(record)
        return output[:12000]

    def _format_exec(self, res: dict[str, Any]) -> str:
        return f"exit={res.get('exit')}\nstdout:\n{res.get('stdout','')}\nstderr:\n{res.get('stderr','')}"

    def _tool_specs(self, include_lsp: bool) -> list[dict[str, Any]]:
        def obj(props: dict[str, Any], required: list[str]) -> dict[str, Any]:
            return {"type": "object", "properties": props, "required": required, "additionalProperties": False}
        specs = [
            {"type": "function", "function": {"name": "read_file", "description": "Read a file from the sandbox.", "parameters": obj({"path": {"type": "string"}}, ["path"])}},
            {"type": "function", "function": {"name": "write_file", "description": "Overwrite a file in the sandbox.", "parameters": obj({"path": {"type": "string"}, "content": {"type": "string"}}, ["path", "content"])}},
            {"type": "function", "function": {"name": "list_dir", "description": "List directory entries.", "parameters": obj({"path": {"type": "string"}, "timeout": {"type": "integer", "default": 30}}, ["path"])}},
            {"type": "function", "function": {"name": "exec", "description": "Run a shell command in the sandbox.", "parameters": obj({"cmd": {"type": "string"}, "timeout": {"type": "integer", "default": 30}}, ["cmd"])}},
            {"type": "function", "function": {"name": "submit", "description": "Run the failing test and report pass/fail.", "parameters": obj({}, [])}},
        ]
        if include_lsp:
            specs.extend([
                {"type": "function", "function": {"name": "lsp_goto_def", "description": "Jump to a symbol's definition AND return surrounding code. One call replaces lsp_definition + read_file.", "parameters": obj({"file": {"type": "string"}, "line": {"type": "integer", "description": "zero-based"}, "character": {"type": "integer", "description": "zero-based"}, "context_lines": {"type": "integer", "default": 12}}, ["file", "line", "character"])}},
                {"type": "function", "function": {"name": "lsp_references", "description": "All call sites of the symbol at the given position. Returns 'path:line:col' strings.", "parameters": obj({"file": {"type": "string"}, "line": {"type": "integer"}, "character": {"type": "integer"}}, ["file", "line", "character"])}},
                {"type": "function", "function": {"name": "lsp_outline", "description": "Symbols (functions/classes/methods) defined in a file.", "parameters": obj({"file": {"type": "string"}}, ["file"])}},
                {"type": "function", "function": {"name": "lsp_find_symbol", "description": "Find where a symbol (function/class/constant) is defined anywhere in the workspace. Returns {name, kind, loc, preview} per hit. Use this FIRST when the failing test names a symbol you don't have in front of you.", "parameters": obj({"query": {"type": "string"}}, ["query"])}},
            ])
        return specs
