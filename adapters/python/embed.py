"""Embedding search over a repository's functions and classes.

  embed.py build REPO INDEX.db OUT.npz [VECCACHE.db]
      embed every non-test symbol in INDEX.db (built by index.py) with a
      local model; VECCACHE.db reuses vectors across versions of a repository
  embed.py search OUT.npz QUERY [N]
      nearest symbols by cosine similarity

Needs fastembed and numpy (a separate interpreter from the repository's).
The model runs locally; nothing is sent anywhere.
"""
import hashlib
import os
import sqlite3
import sys

import numpy as np

MODEL = "BAAI/bge-small-en-v1.5"
QUERY_PREFIX = "Represent this sentence for searching relevant passages: "


def model():
    from fastembed import TextEmbedding
    return TextEmbedding(MODEL)


def is_test(path):
    return "/tests/" in path or path.startswith("tests/") or os.path.basename(path).startswith("test_")


def build(repo, index, out, cache=None):
    con = sqlite3.connect(index)
    rows = [r for r in con.execute("select path, qual, kind, start, end, sig, doc from sym") if not is_test(r[0])]
    files = {}
    texts, meta = [], []
    for path, qual, kind, start, end, sig, doc in rows:
        if path not in files:
            try:
                files[path] = open(os.path.join(repo, path), encoding="utf-8", errors="replace").read().split("\n")
            except OSError:
                files[path] = []
        body = "\n".join(files[path][start - 1:min(end, start + 30)])
        texts.append(f"{path} {qual}\n{body}"[:1200])
        meta.append(f"{path}:{start}-{end}  {kind} {qual}\n    {sig}" + (f"\n    {' '.join(doc.split())[:160]}" if doc else ""))
    keys = [hashlib.sha1(t.encode()).hexdigest() for t in texts]
    vecs = [None] * len(texts)
    cc = None
    if cache:
        cc = sqlite3.connect(cache, timeout=120)
        cc.execute("create table if not exists v(k text primary key, v blob)")
        for i in range(0, len(keys), 500):
            part = keys[i:i + 500]
            for k, v in cc.execute("select k, v from v where k in (%s)" % ",".join("?" * len(part)), part):
                for j in range(i, i + len(part)):
                    if keys[j] == k:
                        vecs[j] = np.frombuffer(v, dtype=np.float32)
    todo = [i for i, v in enumerate(vecs) if v is None]
    if todo:
        m = model()
        for i, v in zip(todo, m.embed([texts[i] for i in todo], batch_size=64)):
            vecs[i] = np.asarray(v, dtype=np.float32)
        if cc:
            cc.executemany("insert or replace into v values (?,?)", [(keys[i], vecs[i].tobytes()) for i in todo])
            cc.commit()
    mat = np.vstack(vecs) if vecs else np.zeros((0, 384), dtype=np.float32)
    mat /= np.linalg.norm(mat, axis=1, keepdims=True) + 1e-9
    np.savez(out, vecs=mat, meta=np.array(meta, dtype=object))
    print(f"embedded {len(texts)} symbols ({len(todo)} new)")


def search(out, query, n=12):
    z = np.load(out, allow_pickle=True)
    q = np.asarray(next(iter(model().embed([QUERY_PREFIX + query]))), dtype=np.float32)
    q /= np.linalg.norm(q) + 1e-9
    score = z["vecs"] @ q
    for i in np.argsort(-score)[:n]:
        print(z["meta"][i])


if __name__ == "__main__":
    cmd, a = sys.argv[1], sys.argv[2:]
    if cmd == "build":
        build(a[0], a[1], a[2], a[3] if len(a) > 3 else None)
    elif cmd == "search":
        search(a[0], a[1], int(a[2]) if len(a) > 2 else 12)
    else:
        sys.exit("unknown command " + cmd)
