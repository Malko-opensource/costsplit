#!/usr/bin/env node
// TypeScript language adapter for graybox.
//
// Protocol: `node adapter.mjs <op>` reads one JSON object on stdin and prints
// one JSON object. The working directory is the project root.
//
// Symbols look like "src/service.ts#Service.apply" or "src/make.ts#makeService".
// JavaScript is accepted only as TypeScript or JSDoc + checkJs (D-019), so the
// project's tsconfig.json must have "strict": true.
import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import ts from "typescript";

const ROOT = process.cwd();
const HERE = path.dirname(fileURLToPath(import.meta.url));
const SKIP = new Set(["node_modules", ".graybox", ".git", "dist", "build"]);

const out = (o) => process.stdout.write(JSON.stringify(o) + "\n");
const norm = (s) => s.replace(/\s+/g, " ").trim();
const tail = (s, n = 2000) => (s.length <= n ? s : "..." + s.slice(-n));

// ---------- files ----------

function tsFiles(dir = ROOT) {
  const res = [];
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    if (e.isDirectory()) {
      if (!SKIP.has(e.name) && !e.name.startsWith(".")) res.push(...tsFiles(path.join(dir, e.name)));
    } else if (/\.(ts|tsx|mts|cts|js|mjs)$/.test(e.name) && !e.name.endsWith(".d.ts")) {
      res.push(path.join(dir, e.name));
    }
  }
  return res.sort();
}

const isTestFile = (p) => /\.(test|spec)\.[cm]?[jt]sx?$/.test(p) || path.relative(ROOT, p).split(path.sep)[0] === "test";

function parse(file, text = fs.readFileSync(file, "utf8")) {
  return ts.createSourceFile(file, text, ts.ScriptTarget.Latest, true);
}

// ---------- symbols ----------

function find(symbol) {
  const hash = symbol.indexOf("#");
  if (hash <= 0) throw new LookupError(`bad TS symbol ${symbol}: want path/file.ts#Class.method or #func`);
  const file = path.join(ROOT, symbol.slice(0, hash));
  if (!fs.existsSync(file)) throw new LookupError(`file ${symbol.slice(0, hash)} not found`);
  const parts = symbol.slice(hash + 1).split(".");
  const text = fs.readFileSync(file, "utf8");
  const sf = parse(file, text);
  let scope = sf.statements;
  for (let i = 0; i < parts.length; i++) {
    const last = i === parts.length - 1;
    let match;
    for (const n of scope) {
      const name = n.name && ts.isIdentifier(n.name) ? n.name.text : undefined;
      if (name !== parts[i]) continue;
      if (last && (ts.isFunctionDeclaration(n) || ts.isMethodDeclaration(n))) match = n;
      if (!last && ts.isClassDeclaration(n)) match = n;
    }
    if (!match) throw new LookupError(`symbol ${symbol} not found`);
    if (last) return { file, text, sf, node: match };
    scope = match.members;
  }
  throw new LookupError(`symbol ${symbol} not found`);
}

class LookupError extends Error {}

function signature(node, sf) {
  const full = node.getText(sf);
  return node.body ? norm(full.slice(0, node.body.getStart(sf) - node.getStart(sf))) : norm(full);
}

function isStub(node, sf) {
  const st = node.body?.statements;
  if (!st || st.length !== 1) return false;
  const s = st[0];
  if (ts.isThrowStatement(s) && s.expression && ts.isNewExpression(s.expression)) {
    const arg = s.expression.arguments?.[0];
    return s.expression.expression.getText(sf) === "Error" && arg && ts.isStringLiteral(arg) && arg.text === "not implemented";
  }
  if (ts.isExpressionStatement(s) && ts.isCallExpression(s.expression)) {
    const callee = s.expression.expression;
    return ts.isPropertyAccessExpression(callee) && callee.name.text === "skip";
  }
  return false;
}

const ops = {
  resolve({ symbol }) {
    const { file, sf, node } = find(symbol);
    return { symbol: { name: symbol, file: path.relative(ROOT, file), is_stub: isStub(node, sf), signature: signature(node, sf) } };
  },

  apply({ symbol, body }) {
    const { file, text, sf, node } = find(symbol);
    if (!node.body) return { error: `${symbol} has no body` };
    const lineStart = text.lastIndexOf("\n", node.getStart(sf)) + 1;
    const declIndent = text.slice(lineStart, node.getStart(sf)).match(/^\s*/)[0];
    const inner = declIndent + "  ";
    const lines = body.replace(/^\n+|\n+$/g, "").split("\n");
    const minIndent = Math.min(...lines.filter((l) => l.trim()).map((l) => l.match(/^\s*/)[0].length));
    const indented = lines.map((l) => (l.trim() ? inner + l.slice(minIndent) : "")).join("\n");
    const open = node.body.getStart(sf) + 1;
    const close = node.body.getEnd() - 1;
    const next = text.slice(0, open) + "\n" + indented + "\n" + declIndent + text.slice(close);

    const syntax = ts.transpileModule(next, { reportDiagnostics: true, fileName: file }).diagnostics ?? [];
    if (syntax.length) return { error: "body does not parse: " + ts.flattenDiagnosticMessageText(syntax[0].messageText, "\n") };
    fs.writeFileSync(file, next);
    const after = find(symbol);
    if (signature(after.node, after.sf) !== signature(node, sf)) {
      fs.writeFileSync(file, text);
      return { error: "apply would change the signature" };
    }
    return {};
  },

  preconditions() {
    const cfgPath = ts.findConfigFile(ROOT, ts.sys.fileExists, "tsconfig.json");
    if (!cfgPath) return { checks: [{ name: "strict_typing", status: "fail", detail: "tsconfig.json missing (D-019)" }] };
    const cfg = ts.readConfigFile(cfgPath, ts.sys.readFile);
    const parsed = ts.parseJsonConfigFileContent(cfg.config, ts.sys, path.dirname(cfgPath));
    const o = parsed.options;
    if (!o.strict) return { checks: [{ name: "strict_typing", status: "fail", detail: 'tsconfig "strict" must be true (D-019)' }] };
    if (o.allowJs && !o.checkJs) return { checks: [{ name: "strict_typing", status: "fail", detail: "allowJs requires checkJs (D-019)" }] };
    return { checks: [{ name: "strict_typing", status: "pass", detail: "" }] };
  },

  checks() {
    const checks = [];
    const bad = [];
    for (const f of tsFiles()) {
      const d = ts.transpileModule(fs.readFileSync(f, "utf8"), { reportDiagnostics: true, fileName: f }).diagnostics ?? [];
      if (d.length) bad.push(`${path.relative(ROOT, f)}: ${ts.flattenDiagnosticMessageText(d[0].messageText, "\n")}`);
    }
    checks.push({ name: "syntax", status: bad.length ? "fail" : "pass", detail: bad.join("\n") });
    if (bad.length) return { checks };

    const local = path.join(ROOT, "node_modules/.bin/tsc");
    const tsc = fs.existsSync(local) ? local : path.join(HERE, "node_modules/.bin/tsc");
    let r = spawnSync(tsc, ["--noEmit", "-p", "."], { cwd: ROOT, encoding: "utf8" });
    checks.push({ name: "typecheck", status: r.status === 0 ? "pass" : "fail", detail: r.status === 0 ? "" : tail(r.stdout + r.stderr) });

    const eslint = path.join(ROOT, "node_modules/.bin/eslint");
    if (fs.existsSync(eslint)) {
      r = spawnSync(eslint, ["."], { cwd: ROOT, encoding: "utf8" });
      checks.push({ name: "lint", status: r.status === 0 ? "pass" : "fail", detail: r.status === 0 ? "" : tail(r.stdout + r.stderr) });
    } else {
      checks.push({ name: "lint", status: "skipped", detail: "eslint not installed in project" });
    }

    const tests = tsFiles().filter(isTestFile).map((f) => path.relative(ROOT, f));
    r = spawnSync(process.execPath, ["--test", ...tests], { cwd: ROOT, encoding: "utf8" });
    checks.push({ name: "test", status: r.status === 0 ? "pass" : "fail", detail: r.status === 0 ? "" : tail(r.stdout + r.stderr) });
    return { checks };
  },

  list_tests() {
    const names = [];
    for (const f of tsFiles().filter(isTestFile)) {
      const sf = parse(f);
      const visit = (n) => {
        if (ts.isCallExpression(n) && ts.isIdentifier(n.expression) && ["test", "it"].includes(n.expression.text)) {
          const a = n.arguments[0];
          if (a && (ts.isStringLiteral(a) || ts.isNoSubstitutionTemplateLiteral(a))) names.push(a.text);
        }
        ts.forEachChild(n, visit);
      };
      visit(sf);
    }
    return { tests: names.sort() };
  },

  api_snapshot() {
    const lines = [];
    const exported = (n) => n.modifiers?.some((m) => m.kind === ts.SyntaxKind.ExportKeyword);
    const privateMember = (m) =>
      (m.name && ts.isPrivateIdentifier(m.name)) ||
      m.modifiers?.some((x) => x.kind === ts.SyntaxKind.PrivateKeyword);
    for (const f of tsFiles()) {
      if (isTestFile(f)) continue;
      const rel = path.relative(ROOT, f);
      const sf = parse(f);
      for (const n of sf.statements) {
        if (!exported(n)) continue;
        if (ts.isFunctionDeclaration(n)) {
          lines.push(`${rel}: ${signature(n, sf)}`);
        } else if (ts.isClassDeclaration(n)) {
          const head = n.getText(sf).slice(0, n.members.pos - n.getStart(sf));
          lines.push(`${rel}: ${norm(head.replace(/\{\s*$/, ""))}`);
          for (const m of n.members) {
            if (privateMember(m)) continue;
            const name = m.name ? m.name.getText(sf) : "constructor";
            lines.push(`${rel}: ${n.name.text}.${name}: ${signature(m, sf)}`);
          }
        } else {
          // interfaces, type aliases, exported consts (e.g. the transition table)
          lines.push(`${rel}: ${norm(n.getText(sf))}`);
        }
      }
    }
    return { lines: lines.sort() };
  },
};

const op = process.argv.at(-1);
if (!ops[op]) {
  out({ error: `usage: adapter.mjs ${Object.keys(ops).join("|")}` });
  process.exit(2);
}
let input = "";
process.stdin.on("data", (c) => (input += c));
process.stdin.on("end", () => {
  try {
    out(ops[op](JSON.parse(input || "{}")));
  } catch (e) {
    if (e instanceof LookupError) out({ error: e.message });
    else throw e;
  }
});
