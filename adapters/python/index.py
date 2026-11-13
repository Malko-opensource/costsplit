"""A SQLite index of a Python repository: symbols, call edges, full text.

  index.py build REPO DB           parse every .py under REPO into DB
  index.py search DB QUERY [N]     symbols whose name or docstring match
  index.py symbol DB NAME          source of the definitions named NAME
  index.py callers DB NAME [N]     functions that call NAME
  index.py callees DB NAME         what the definitions named NAME call

Output is plain text for an agent to read. Standard library only.
"""
import ast
import os
import re
import sqlite3
import sys

SKIP = {".git", "node_modules", "__pycache__", "build", "dist", ".tox", "venv", ".venv"}


def build(repo, db):
    if os.path.exists(db):
        os.remove(db)
    con = sqlite3.connect(db)
    con.executescript("""
        create table sym(id integer primary key, path text, qual text, name text, kind text, start int, end int, sig text, doc text);
        create table call(caller int, callee text);
        create index sym_name on sym(name);
        create index call_callee on call(callee);
        create virtual table fts using fts5(qual, words, doc, content='');
    """)
    for root, dirs, files in os.walk(repo):
        dirs[:] = sorted(d for d in dirs if d not in SKIP)
        for f in sorted(files):
            if not f.endswith(".py"):
                continue
            full = os.path.join(root, f)
            path = os.path.relpath(full, repo)
            try:
                src = open(full, encoding="utf-8", errors="replace").read()
                tree = ast.parse(src)
            except (SyntaxError, ValueError, RecursionError):
                continue
            lines = src.split("\n")
            walk(con, path, lines, tree, "")
    con.commit()
    n = con.execute("select count(*) from sym").fetchone()[0]
    print(f"indexed {n} symbols")


def words(name):
    parts = re.findall(r"[A-Za-z][a-z0-9]*|[0-9]+", name.replace("_", " "))
    return " ".join(p.lower() for p in parts)


def walk(con, path, lines, node, prefix):
    for ch in ast.iter_child_nodes(node):
        if isinstance(ch, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            qual = prefix + ch.name
            kind = "class" if isinstance(ch, ast.ClassDef) else "def"
            start = min([ch.lineno] + [d.lineno for d in ch.decorator_list])
            end = getattr(ch, "end_lineno", ch.lineno)
            sig = lines[ch.lineno - 1].strip()
            doc = (ast.get_docstring(ch) or "").strip().split("\n\n")[0][:300]
            cur = con.execute("insert into sym(path, qual, name, kind, start, end, sig, doc) values (?,?,?,?,?,?,?,?)",
                              (path, qual, ch.name, kind, start, end, sig, doc))
            sid = cur.lastrowid
            con.execute("insert into fts(rowid, qual, words, doc) values (?,?,?,?)", (sid, qual, words(qual), doc))
            if kind == "def":
                seen = set()
                for c in ast.walk(ch):
                    if isinstance(c, ast.Call):
                        fn = c.func
                        name = fn.id if isinstance(fn, ast.Name) else fn.attr if isinstance(fn, ast.Attribute) else None
                        if name and name not in seen:
                            seen.add(name)
                            con.execute("insert into call values (?,?)", (sid, name))
            walk(con, path, lines, ch, qual + ".")


def row(r):
    path, qual, kind, start, end, sig, doc = r
    out = f"{path}:{start}-{end}  {kind} {qual}\n    {sig}"
    if doc:
        out += "\n    " + " ".join(doc.split())[:160]
    return out


COLS = "path, qual, kind, start, end, sig, doc"


def search(db, query, n=15):
    con = sqlite3.connect(db)
    terms = [t for t in re.findall(r"[A-Za-z0-9_]+", query) if len(t) > 1]
    if not terms:
        print("empty query")
        return
    rows = []
    # Exact and prefix name matches first, then full text over names and docstrings.
    for t in terms:
        rows += con.execute(f"select {COLS} from sym where name = ? limit 8", (t,)).fetchall()
    q = " OR ".join('"' + w + '"' for t in terms for w in (words(t).split() or [t.lower()]))
    try:
        rows += con.execute(f"select {COLS} from sym where id in (select rowid from fts where fts match ? order by bm25(fts, 4.0, 4.0, 1.0) limit ?)", (q, n * 2)).fetchall()
    except sqlite3.OperationalError as e:
        print("search error:", e)
    seen, out = set(), []
    for r in rows:
        if r[:4] not in seen and not is_test(r[0]):
            seen.add(r[:4])
            out.append(r)
    for r in rows:
        if r[:4] not in seen:
            seen.add(r[:4])
            out.append(r)
    if not out:
        print("no symbols match")
    for r in out[:n]:
        print(row(r))


def is_test(path):
    return "/tests/" in path or path.startswith("tests/") or os.path.basename(path).startswith("test_")


def named(con, name):
    rows = con.execute(f"select id, {COLS} from sym where qual = ?", (name,)).fetchall()
    if not rows:
        rows = con.execute(f"select id, {COLS} from sym where name = ? or qual like ?", (name.split(".")[-1], "%." + name)).fetchall()
    return rows


def symbol(db, repo, name):
    con = sqlite3.connect(db)
    rows = named(con, name)
    if not rows:
        print("no symbol named", name)
        return
    budget = 400
    for r in rows[:6]:
        _, path, qual, kind, start, end, _, _ = r
        print(f"# {path}:{start}-{end}  {kind} {qual}")
        try:
            lines = open(os.path.join(repo, path), encoding="utf-8", errors="replace").read().split("\n")
        except OSError:
            continue
        stop = min(end, start + budget - 1)
        for i in range(start, stop + 1):
            print(f"{i:6d}| {lines[i - 1]}")
        if stop < end:
            print(f"… [lines {stop + 1}-{end} not shown; read_file {path} for the rest]")
        budget -= stop - start + 1
        print()
        if budget <= 0:
            break
    if len(rows) > 6:
        print(f"… {len(rows) - 6} more definitions named {name}; use search to list them")


def callers(db, name, n=25):
    con = sqlite3.connect(db)
    rows = con.execute(f"select {COLS} from sym where id in (select caller from call where callee = ?) limit ?", (name.split(".")[-1], n * 3)).fetchall()
    rows = [r for r in rows if not is_test(r[0])] + [r for r in rows if is_test(r[0])]
    if not rows:
        print("no callers found (calls are matched by name only)")
    for r in rows[:n]:
        print(f"{r[0]}:{r[3]}-{r[4]}  {r[2]} {r[1]}")


def callees(db, name):
    con = sqlite3.connect(db)
    rows = named(con, name)
    if not rows:
        print("no symbol named", name)
    for r in rows[:4]:
        names = [x[0] for x in con.execute("select callee from call where caller = ?", (r[0],))]
        print(f"{r[1]}:{r[4]}  {r[2]} calls: {', '.join(sorted(names)) or '(nothing)'}")


if __name__ == "__main__":
    cmd, a = sys.argv[1], sys.argv[2:]
    if cmd == "build":
        build(a[0], a[1])
    elif cmd == "search":
        search(a[0], a[1], int(a[2]) if len(a) > 2 else 15)
    elif cmd == "symbol":
        symbol(a[0], a[1], a[2])
    elif cmd == "callers":
        callers(a[0], a[1], int(a[2]) if len(a) > 2 else 25)
    elif cmd == "callees":
        callees(a[0], a[1])
    else:
        sys.exit("unknown command " + cmd)
