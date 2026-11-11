#!/usr/bin/env python3
"""Python language adapter for graybox (stdlib only).

Protocol: `adapter.py <op>` reads one JSON object on stdin and prints one JSON
object. The working directory is the project root.

Symbols look like "order.service:Service.apply" or "order.service:make_service".
Strict type checking with pyright is required (D-019).
"""
from __future__ import annotations

import ast
import json
import os
import shutil
import subprocess
import sys
import textwrap
from pathlib import Path
from typing import Any

ROOT = Path.cwd()
HERE = Path(__file__).resolve().parent
SKIP_DIRS = {".graybox", "node_modules", "__pycache__", ".git", ".venv", "venv"}


def out(obj: dict[str, Any]) -> None:
    json.dump(obj, sys.stdout)
    sys.stdout.write("\n")


# ---------- symbols ----------

def module_file(module: str) -> Path:
    rel = Path(*module.split("."))
    for base in (ROOT, ROOT / "src"):
        for cand in (base / rel.with_suffix(".py"), base / rel / "__init__.py"):
            if cand.is_file():
                return cand
    raise LookupError(f"module {module} not found")


def find(symbol: str) -> tuple[Path, str, ast.FunctionDef | ast.AsyncFunctionDef]:
    if ":" not in symbol:
        raise LookupError(f"bad Python symbol {symbol!r}: want module.path:Qual.name")
    module, qual = symbol.split(":", 1)
    path = module_file(module)
    src = path.read_text()
    tree = ast.parse(src)
    nodes: list[ast.stmt] = tree.body
    parts = qual.split(".")
    for i, part in enumerate(parts):
        last = i == len(parts) - 1
        match = None
        for n in nodes:
            if last and isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and n.name == part:
                match = n
            elif not last and isinstance(n, ast.ClassDef) and n.name == part:
                match = n
        if match is None:
            raise LookupError(f"symbol {symbol} not found")
        if last:
            return path, src, match  # type: ignore[return-value]
        nodes = match.body  # type: ignore[union-attr]
    raise LookupError(f"symbol {symbol} not found")


def body_without_docstring(fn: ast.FunctionDef | ast.AsyncFunctionDef) -> list[ast.stmt]:
    body = fn.body
    if body and isinstance(body[0], ast.Expr) and isinstance(body[0].value, ast.Constant) and isinstance(body[0].value.value, str):
        return body[1:]
    return body


def is_stub(fn: ast.FunctionDef | ast.AsyncFunctionDef) -> bool:
    body = body_without_docstring(fn)
    if len(body) != 1:
        return False
    st = body[0]
    if isinstance(st, ast.Raise) and st.exc is not None:
        exc = st.exc.func if isinstance(st.exc, ast.Call) else st.exc
        name = exc.id if isinstance(exc, ast.Name) else exc.attr if isinstance(exc, ast.Attribute) else ""
        return name in ("NotImplementedError", "SkipTest")
    if isinstance(st, ast.Expr) and isinstance(st.value, ast.Call):
        f = st.value.func
        return isinstance(f, ast.Attribute) and f.attr == "skipTest"
    return False


def signature(fn: ast.FunctionDef | ast.AsyncFunctionDef) -> str:
    prefix = "async def " if isinstance(fn, ast.AsyncFunctionDef) else "def "
    ret = f" -> {ast.unparse(fn.returns)}" if fn.returns else ""
    return f"{prefix}{fn.name}({ast.unparse(fn.args)}){ret}"


def op_resolve(req: dict[str, Any]) -> dict[str, Any]:
    path, _, fn = find(req["symbol"])
    return {"symbol": {"name": req["symbol"], "file": str(path.relative_to(ROOT)), "is_stub": is_stub(fn), "signature": signature(fn)}}


def op_apply(req: dict[str, Any]) -> dict[str, Any]:
    path, src, fn = find(req["symbol"])
    stmts = body_without_docstring(fn)
    if not stmts:
        return {"error": "function has no body to replace"}
    first, last = stmts[0], stmts[-1]
    indent = " " * first.col_offset
    new_body = textwrap.indent(textwrap.dedent(req["body"]).strip("\n"), indent)
    lines = src.splitlines(keepends=True)
    start, end = first.lineno - 1, last.end_lineno or last.lineno
    new_src = "".join(lines[:start]) + new_body + "\n" + "".join(lines[end:])
    try:
        new_tree = ast.parse(new_src)
    except SyntaxError as e:
        return {"error": f"body does not parse: {e}"}
    # The signature must be unchanged (D-014).
    path.write_text(new_src)
    _, _, new_fn = find(req["symbol"])
    if signature(new_fn) != signature(fn):
        path.write_text(src)
        return {"error": "apply would change the signature"}
    del new_tree
    return {}


# ---------- files ----------

def py_files() -> list[Path]:
    files: list[Path] = []
    for dirpath, dirnames, filenames in os.walk(ROOT):
        dirnames[:] = [d for d in dirnames if d not in SKIP_DIRS and not d.startswith(".")]
        for f in filenames:
            if f.endswith(".py"):
                files.append(Path(dirpath) / f)
    return sorted(files)


def is_test_file(p: Path) -> bool:
    rel = p.relative_to(ROOT)
    return rel.parts[0] == "tests" or p.name.startswith("test_")


def module_name(p: Path) -> str:
    rel = p.relative_to(ROOT)
    if rel.parts[0] == "src":
        rel = Path(*rel.parts[1:])
    parts = list(rel.with_suffix("").parts)
    if parts[-1] == "__init__":
        parts = parts[:-1]
    return ".".join(parts)


# ---------- checks ----------

def pyright_cmd() -> list[str] | None:
    env = os.environ.get("GRAYBOX_PYRIGHT")
    if env:
        return [env]
    local = HERE / "node_modules" / ".bin" / "pyright"
    if local.exists():
        return [str(local)]
    found = shutil.which("pyright")
    return [found] if found else None


def run(cmd: list[str]) -> tuple[int, str]:
    p = subprocess.run(cmd, cwd=ROOT, capture_output=True, text=True)
    return p.returncode, (p.stdout + p.stderr).strip()


def tail(s: str, n: int = 2000) -> str:
    return s if len(s) <= n else "..." + s[-n:]


def op_checks(_: dict[str, Any]) -> dict[str, Any]:
    checks: list[dict[str, str]] = []
    bad = []
    for f in py_files():
        try:
            ast.parse(f.read_text(), filename=str(f))
        except SyntaxError as e:
            bad.append(f"{f.relative_to(ROOT)}:{e.lineno}: {e.msg}")
    checks.append({"name": "syntax", "status": "fail" if bad else "pass", "detail": "\n".join(bad)})
    if bad:
        return {"checks": checks}

    if shutil.which("ruff"):
        code, text = run(["ruff", "check", "."])
        checks.append({"name": "lint", "status": "pass" if code == 0 else "fail", "detail": tail(text) if code else ""})
    else:
        checks.append({"name": "lint", "status": "skipped", "detail": "ruff not installed"})

    pr = pyright_cmd()
    if pr is None:
        checks.append({"name": "typecheck", "status": "fail", "detail": "pyright not installed; strict typing is required (D-019)"})
    else:
        code, text = run(pr)
        checks.append({"name": "typecheck", "status": "pass" if code == 0 else "fail", "detail": tail(text) if code else ""})

    test_dir = "tests" if (ROOT / "tests").is_dir() else "."
    code, text = run([sys.executable, "-m", "unittest", "discover", "-s", test_dir, "-t", "."])
    checks.append({"name": "test", "status": "pass" if code == 0 else "fail", "detail": tail(text) if code else ""})
    return {"checks": checks}


def op_preconditions(_: dict[str, Any]) -> dict[str, Any]:
    cfg = ROOT / "pyrightconfig.json"
    if not cfg.is_file():
        return {"checks": [{"name": "strict_typing", "status": "fail", "detail": "pyrightconfig.json missing; strict typing is required (D-019)"}]}
    try:
        mode = json.loads(cfg.read_text()).get("typeCheckingMode")
    except json.JSONDecodeError as e:
        return {"checks": [{"name": "strict_typing", "status": "fail", "detail": f"pyrightconfig.json: {e}"}]}
    if mode != "strict":
        return {"checks": [{"name": "strict_typing", "status": "fail", "detail": f'typeCheckingMode is {mode!r}, must be "strict" (D-019)'}]}
    return {"checks": [{"name": "strict_typing", "status": "pass", "detail": ""}]}


# ---------- tests and API ----------

def op_list_tests(_: dict[str, Any]) -> dict[str, Any]:
    names: list[str] = []
    for f in py_files():
        if not is_test_file(f):
            continue
        tree = ast.parse(f.read_text())
        for n in ast.walk(tree):
            if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and n.name.startswith("test"):
                names.append(n.name)
    return {"tests": sorted(names)}


def public(name: str) -> bool:
    return not name.startswith("_") or (name.startswith("__") and name.endswith("__"))


def op_api_snapshot(_: dict[str, Any]) -> dict[str, Any]:
    lines: list[str] = []
    for f in py_files():
        if is_test_file(f):
            continue
        mod = module_name(f)
        tree = ast.parse(f.read_text())
        for n in tree.body:
            if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and public(n.name):
                lines.append(f"{mod}:{signature(n)}")
            elif isinstance(n, ast.ClassDef) and public(n.name):
                deco = "".join(f"@{ast.unparse(d)} " for d in n.decorator_list)
                bases = ", ".join(ast.unparse(b) for b in n.bases + n.keywords)  # type: ignore[operator]
                lines.append(f"{mod}:{deco}class {n.name}({bases})")
                for m in n.body:
                    if isinstance(m, (ast.FunctionDef, ast.AsyncFunctionDef)) and public(m.name):
                        lines.append(f"{mod}:{n.name}.{signature(m)}")
                    elif isinstance(m, ast.AnnAssign) and isinstance(m.target, ast.Name):
                        val = f" = {ast.unparse(m.value)}" if m.value else ""
                        lines.append(f"{mod}:{n.name}.{m.target.id}: {ast.unparse(m.annotation)}{val}")
                    elif isinstance(m, ast.Assign):
                        lines.append(f"{mod}:{n.name}.{ast.unparse(m)}")
            elif isinstance(n, ast.AnnAssign) and isinstance(n.target, ast.Name) and public(n.target.id):
                val = f" = {ast.unparse(n.value)}" if n.value else ""
                lines.append(f"{mod}:{n.target.id}: {ast.unparse(n.annotation)}{val}")
            elif isinstance(n, ast.Assign) and all(isinstance(t, ast.Name) and public(t.id) for t in n.targets):
                lines.append(f"{mod}:{ast.unparse(n)}")
    return {"lines": sorted(" ".join(l.split()) for l in lines)}


OPS = {
    "resolve": op_resolve,
    "apply": op_apply,
    "checks": op_checks,
    "preconditions": op_preconditions,
    "list_tests": op_list_tests,
    "api_snapshot": op_api_snapshot,
}


def main() -> None:
    if len(sys.argv) < 2 or sys.argv[-1] not in OPS:
        out({"error": f"usage: adapter.py {'|'.join(OPS)}"})
        sys.exit(2)
    raw = sys.stdin.read() or "{}"
    try:
        out(OPS[sys.argv[-1]](json.loads(raw)))
    except LookupError as e:
        out({"error": str(e)})


if __name__ == "__main__":
    main()
