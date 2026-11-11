#!/usr/bin/env node
// TypeScript language adapter for graybox.
//
// Protocol: `node adapter.mjs <op>` reads one JSON object on stdin and prints
// one JSON object. The working directory is the project root.
//
// Symbols look like "src/service.ts#Service.apply", "src/make.ts#makeService",
// "affine-cipher.js#encode" (export const encode = () => {}), "x.js#C.constructor",
// or a top-level test "x.test.mjs#test:REQ-001: encodes yes".
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

class LookupError extends Error {}

const isFn = (n) => n && (ts.isArrowFunction(n) || ts.isFunctionExpression(n));

// entries lists every function-like declaration in a file:
// { qual, fn (node with .body), head (node where the declaration starts) }.
// Covers `function f`, `export const f = () => {}`, class methods,
// constructors ("C.constructor") and arrow-function class properties.
function entries(sf) {
  const out = [];
  for (const n of sf.statements) {
    if (ts.isFunctionDeclaration(n) && n.name) out.push({ qual: n.name.text, fn: n, head: n });
    if (ts.isVariableStatement(n)) {
      for (const d of n.declarationList.declarations) {
        if (ts.isIdentifier(d.name) && isFn(d.initializer)) out.push({ qual: d.name.text, fn: d.initializer, head: n });
      }
    }
    // test("name", (t) => { ... }) at top level → "test:name"
    if (ts.isExpressionStatement(n) && ts.isCallExpression(n.expression)) {
      const c = n.expression;
      const a0 = c.arguments[0];
      const cb = c.arguments.find(isFn);
      if (ts.isIdentifier(c.expression) && ["test", "it"].includes(c.expression.text) && a0 && ts.isStringLiteralLike(a0) && cb)
        out.push({ qual: `test:${a0.text}`, fn: cb, head: n });
    }
    if (ts.isClassDeclaration(n) && n.name) {
      for (const m of n.members) {
        if (ts.isConstructorDeclaration(m)) out.push({ qual: `${n.name.text}.constructor`, fn: m, head: m });
        else if (ts.isMethodDeclaration(m) && m.name && ts.isIdentifier(m.name)) out.push({ qual: `${n.name.text}.${m.name.text}`, fn: m, head: m });
        else if (ts.isPropertyDeclaration(m) && m.name && ts.isIdentifier(m.name) && isFn(m.initializer))
          out.push({ qual: `${n.name.text}.${m.name.text}`, fn: m.initializer, head: m });
      }
    }
  }
  return out;
}

function find(symbol) {
  const hash = symbol.indexOf("#");
  if (hash <= 0) throw new LookupError(`bad TS/JS symbol ${symbol}: want path/file.ts#Class.method or path/file.js#func`);
  const file = path.join(ROOT, symbol.slice(0, hash));
  if (!fs.existsSync(file)) throw new LookupError(`file ${symbol.slice(0, hash)} not found`);
  const text = fs.readFileSync(file, "utf8");
  const sf = parse(file, text);
  const hits = entries(sf).filter((e) => e.qual === symbol.slice(hash + 1));
  if (hits.length > 1) throw new LookupError(`${symbol} is ambiguous (overloads are not supported)`);
  if (!hits.length) throw new LookupError(`symbol ${symbol} not found`);
  return { file, text, sf, ...hits[0] };
}

function signatureOf(e, sf) {
  const start = e.head.getStart(sf);
  const end = e.fn.body ? e.fn.body.getStart(sf) : e.head.getEnd();
  return norm(sf.text.slice(start, end));
}

function isStub(e, sf) {
  const body = e.fn.body;
  if (!body || !ts.isBlock(body) || body.statements.length !== 1) return false;
  const s = body.statements[0];
  if (ts.isThrowStatement(s) && s.expression && ts.isNewExpression(s.expression)) {
    const arg = s.expression.arguments?.[0];
    return s.expression.expression.getText(sf) === "Error" && !!arg && (ts.isStringLiteral(arg) || ts.isNoSubstitutionTemplateLiteral(arg));
  }
  if (ts.isExpressionStatement(s) && ts.isCallExpression(s.expression)) {
    const callee = s.expression.expression;
    return ts.isPropertyAccessExpression(callee) && callee.name.text === "skip";
  }
  return false;
}

const ops = {
  resolve({ symbol }) {
    const e = find(symbol);
    return { symbol: { name: symbol, file: path.relative(ROOT, e.file), is_stub: isStub(e, e.sf), signature: signatureOf(e, e.sf) } };
  },

  stubs() {
    const out = [];
    for (const f of tsFiles()) {
      const sf = parse(f);
      for (const e of entries(sf)) if (isStub(e, sf)) out.push(`${path.relative(ROOT, f)}#${e.qual}`);
    }
    return { stubs: out.sort() };
  },

  apply({ symbol, body }) {
    const e = find(symbol);
    const { file, text, sf } = e;
    if (!e.fn.body) return { error: `${symbol} has no body` };
    const headStart = e.head.getStart(sf);
    const lineStart = text.lastIndexOf("\n", headStart) + 1;
    const declIndent = text.slice(lineStart, headStart).match(/^\s*/)[0];
    const inner = declIndent + "  ";
    const lines = body.replace(/^\n+|\n+$/g, "").split("\n");
    const minIndent = Math.min(...lines.filter((l) => l.trim()).map((l) => l.match(/^\s*/)[0].length));
    const indented = lines.map((l) => (l.trim() ? inner + l.slice(minIndent) : "")).join("\n");
    let next;
    if (ts.isBlock(e.fn.body)) {
      const open = e.fn.body.getStart(sf) + 1;
      const close = e.fn.body.getEnd() - 1;
      next = text.slice(0, open) + "\n" + indented + "\n" + declIndent + text.slice(close);
    } else {
      // expression-bodied arrow: turn it into a block
      next = text.slice(0, e.fn.body.getStart(sf)) + "{\n" + indented + "\n" + declIndent + "}" + text.slice(e.fn.body.getEnd());
    }

    const syntax = ts.transpileModule(next, { reportDiagnostics: true, fileName: file }).diagnostics ?? [];
    if (syntax.length) return { error: "body does not parse: " + ts.flattenDiagnosticMessageText(syntax[0].messageText, "\n") };
    fs.writeFileSync(file, next);
    const after = find(symbol);
    if (signatureOf(after, after.sf) !== signatureOf(e, sf)) {
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
          lines.push(`${rel}: ${signatureOf({ fn: n, head: n }, sf)}`);
        } else if (ts.isClassDeclaration(n)) {
          const head = n.getText(sf).slice(0, n.members.pos - n.getStart(sf));
          lines.push(`${rel}: ${norm(head.replace(/\{\s*$/, ""))}`);
          for (const m of n.members) {
            if (privateMember(m)) continue;
            const name = m.name ? m.name.getText(sf) : "constructor";
            const sig = m.body ? signatureOf({ fn: m, head: m }, sf) : norm(m.getText(sf));
            lines.push(`${rel}: ${n.name.text}.${name}: ${sig}`);
          }
        } else if (ts.isVariableStatement(n) && n.declarationList.declarations.some((d) => isFn(d.initializer))) {
          for (const e of entries(sf).filter((x) => x.head === n)) lines.push(`${rel}: ${signatureOf(e, sf)}`);
        } else {
          // interfaces, type aliases, exported consts (e.g. the transition table)
          lines.push(`${rel}: ${norm(n.getText(sf))}`);
        }
        // JSDoc carries the types of JavaScript files (D-019): changing it changes the contract.
        for (const d of ts.getJSDocCommentsAndTags(n)) if (ts.isJSDoc(d)) lines.push(`${rel}: jsdoc ${norm(d.getText(sf))}`);
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
