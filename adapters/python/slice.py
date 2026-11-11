#!/usr/bin/env python3
"""Context slicer: the part of a Python file a fix needs, instead of the file.

usage: slice.py <file> [--lines 12,40] [--names Class.method,func] [--outline lean] [--strict]
       slice.py <file> --list

Selects the innermost functions around the given lines, or the functions with
the given names, and prints JSON:
  head      module-level imports
  outline   signatures: every top-level definition, and the members of the
            classes that hold selected code; with --outline lean, only the
            class lines that enclose selected code
  blocks    complete source of the selected functions (label, start, end, text)
  related   same-file functions the selected code calls, for reference
  added     names that do not exist yet (a method the fix would add): the
            start of the class and its most similarly named members are shown
  missing   names that matched nothing
With --strict a name must match exactly; nothing is guessed.
With --list it prints only the file's symbols (q, cls, start, end): the choices
a model is allowed to pick from when it is asked where a fix goes.
Stdlib only; a reading aid, not an analysis: calls are matched by name.
"""
from __future__ import annotations

import ast
import difflib
import json
import sys

WINDOW = 12        # lines shown around a change that sits outside any function
RELATED_EACH = 60  # longest callee worth showing
RELATED_ALL = 200  # lines of callees in total
CLASS_HEAD = 40    # lines shown of a class selected by name when it is long
CLASS_ANCHOR = 8   # lines shown at the start of a class that lacks a named member
SIBLING_MAX = 80   # longest sibling shown as an example for a member to be added
MEMBER_LIST = 60   # a class with more members lists only the nearest names


def main() -> None:
    path = sys.argv[1]
    want_lines: list[int] = []
    want_names: list[str] = []
    lean = False
    strict = "--strict" in sys.argv[2:]
    args = sys.argv[2:]
    for i, a in enumerate(args[:-1]):
        if a == "--lines":
            want_lines = [int(x) for x in args[i + 1].split(",") if x]
        if a == "--names":
            want_names = [x for x in args[i + 1].split(",") if x]
        if a == "--outline":
            lean = args[i + 1] == "lean"
    src = open(path, encoding="utf-8", errors="replace").read()
    text = src.split("\n")
    tree = ast.parse(src)

    defs: list[dict] = []

    def walk(node: ast.AST, prefix: str, parent: int | None) -> None:
        for ch in ast.iter_child_nodes(node):
            if isinstance(ch, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                start = min([ch.lineno] + [d.lineno for d in ch.decorator_list])
                defs.append({"q": prefix + ch.name, "name": ch.name, "cls": isinstance(ch, ast.ClassDef), "start": start,
                             "line": ch.lineno, "end": ch.end_lineno or ch.lineno, "parent": parent, "node": ch,
                             "depth": 0 if parent is None else defs[parent]["depth"] + 1})
                walk(ch, prefix + ch.name + ".", len(defs) - 1)
            else:
                walk(ch, prefix, parent)

    walk(tree, "", None)
    if "--list" in sys.argv[2:]:
        json.dump({"symbols": [{"q": d["q"], "cls": d["cls"], "start": d["start"], "end": d["end"]} for d in defs]}, sys.stdout)
        return

    def innermost(line: int) -> int | None:
        best = None
        for i, d in enumerate(defs):
            if d["start"] <= line <= d["end"] and (best is None or d["end"] - d["start"] < defs[best]["end"] - defs[best]["start"]):
                best = i
        return best

    chosen: set[int] = set()
    regions: list[tuple[int, int, str]] = []
    for line in want_lines:
        i = innermost(line)
        if i is not None and not defs[i]["cls"]:
            chosen.add(i)
            continue
        lo, hi = max(1, line - WINDOW), min(len(text), line + WINDOW)
        if i is not None:
            lo, hi = max(lo, defs[i]["start"]), min(hi, defs[i]["end"])
        regions.append((lo, hi, f"lines {lo}-{hi}" + (f" of class {defs[i]['q']}" if i is not None else "")))
    missing: list[str] = []
    added: list[str] = []
    wanted_in: dict[int, list[str]] = {}  # class -> members asked for that it does not have
    for name in want_names:
        hits = [i for i, d in enumerate(defs) if d["q"] == name]
        hits = hits or [i for i, d in enumerate(defs) if d["q"].endswith("." + name)]
        if not hits and "." in name and not strict:
            # Class.member where the class exists and the member does not: the
            # fix most likely adds it. Show where it would go, not the file.
            owner, member = name.rsplit(".", 1)
            owners = [i for i, d in enumerate(defs) if d["cls"] and (d["q"] == owner or d["q"].endswith("." + owner) or d["name"] == owner.split(".")[-1])]
            if owners:
                o = owners[0]
                wanted_in.setdefault(o, []).append(member)
                siblings = {d["name"]: i for i, d in enumerate(defs) if d["parent"] == o and not d["cls"]}
                for near in difflib.get_close_matches(member, list(siblings), n=2, cutoff=0.0):
                    if defs[siblings[near]]["end"] - defs[siblings[near]]["start"] < SIBLING_MAX:
                        chosen.add(siblings[near])
                regions.append((defs[o]["start"], min(defs[o]["end"], defs[o]["line"] + CLASS_ANCHOR),
                                f"{defs[o]['q']} (start of the class; {member} does not exist yet)"))
                added.append(name)
                continue
        hits = hits or [i for i, d in enumerate(defs) if d["name"] == name.split(".")[-1]]
        if not hits and not strict:
            tops = {d["name"]: i for i, d in enumerate(defs) if d["depth"] == 0 and not d["cls"]}
            near_tops = difflib.get_close_matches(name.split(".")[-1], list(tops), n=2, cutoff=0.6)
            if near_tops:
                chosen.update(tops[x] for x in near_tops)
                added.append(name)
                continue
        if not hits:
            missing.append(name)
        for i in hits[:2]:
            d = defs[i]
            if d["cls"] and d["end"] - d["start"] > 200:
                regions.append((d["start"], d["start"] + CLASS_HEAD, f"{d['q']} (first lines of the class)"))
            else:
                chosen.add(i)

    for i in chosen:
        regions.append((defs[i]["start"], defs[i]["end"], defs[i]["q"]))
    regions.sort()
    blocks: list[dict] = []
    for lo, hi, label in regions:
        if blocks and lo <= blocks[-1]["end"] + 1:
            if hi > blocks[-1]["end"]:
                blocks[-1]["end"] = hi
                blocks[-1]["label"] += ", " + label
            continue
        blocks.append({"label": label, "start": lo, "end": hi})
    for b in blocks:
        b["text"] = "\n".join(text[b["start"] - 1:b["end"]])

    def shown(d: dict) -> bool:
        return any(b["start"] <= d["start"] and d["end"] <= b["end"] for b in blocks)

    called: list[str] = []
    for i in sorted(chosen):
        for n in ast.walk(defs[i]["node"]):
            if isinstance(n, ast.Call):
                f = n.func
                name = f.id if isinstance(f, ast.Name) else f.attr if isinstance(f, ast.Attribute) else None
                if name and name not in called:
                    called.append(name)
    parents = {defs[i]["parent"] for i in chosen}
    related: list[dict] = []
    budget = RELATED_ALL
    for name in called:
        cands = [d for d in defs if d["name"] == name and not d["cls"] and not shown(d)]
        cands.sort(key=lambda d: d["parent"] not in parents)  # the same class first
        for d in cands[:1]:
            size = d["end"] - d["start"] + 1
            if size <= RELATED_EACH and size <= budget:
                budget -= size
                related.append({"label": d["q"], "start": d["start"], "end": d["end"], "text": "\n".join(text[d["start"] - 1:d["end"]])})

    open_classes = {defs[i]["parent"] for i in chosen} | {i for i, d in enumerate(defs) if d["cls"] and any(d["start"] <= b["start"] <= d["end"] for b in blocks)}
    listed: dict[int, set[str]] = {}  # lean mode: members named for a class that lacks one asked for
    for o, members in wanted_in.items():
        names = [e["name"] for e in defs if e["parent"] == o]
        if len(names) <= MEMBER_LIST:
            listed[o] = set(names)
        else:
            listed[o] = {n for m in members for n in difflib.get_close_matches(m, names, n=MEMBER_LIST // 2, cutoff=0.0)}
    outline: list[str] = []
    for i, d in enumerate(defs):
        if d["depth"] and d["parent"] not in open_classes:
            continue
        if lean and i not in open_classes and d["name"] not in listed.get(d["parent"], ()):
            continue
        sig = text[d["line"] - 1].strip()
        sig = sig if len(sig) <= 110 else sig[:107] + "..."
        note = f"  # L{d['start']}-{d['end']}"
        if d["cls"] and i not in open_classes:
            members = sum(1 for e in defs if e["parent"] == i)
            note += f", {members} members not listed"
        outline.append("    " * d["depth"] + sig + note)

    head = []
    for node in tree.body:
        if isinstance(node, (ast.Import, ast.ImportFrom)):
            head.extend(text[node.lineno - 1:(node.end_lineno or node.lineno)])

    json.dump({"file_lines": len(text), "head": "\n".join(head), "outline": "\n".join(outline), "blocks": blocks,
               "related": related, "added": added, "missing": missing}, sys.stdout)


if __name__ == "__main__":
    main()
