// Java language adapter for graybox.
//
// Run: java -cp lib/javaparser-core.jar JavaAdapter.java <op>   (cwd = project root)
// Reads one JSON object on stdin, prints one JSON object.
//
// Layout: src/main/java, src/test/java (JUnit 6). Symbols look like
// "com.acme.order.Service#apply" (constructors: "Service#<init>"). Stubs are `throw new UnsupportedOperationException(...)`;
// skipped tests call `Assumptions.abort(...)`.
import com.github.javaparser.ParserConfiguration;
import com.github.javaparser.StaticJavaParser;
import com.github.javaparser.ast.CompilationUnit;
import com.github.javaparser.ast.Modifier;
import com.github.javaparser.ast.Node;
import com.github.javaparser.ast.body.*;
import com.github.javaparser.ast.expr.MethodCallExpr;
import com.github.javaparser.ast.expr.ObjectCreationExpr;
import com.github.javaparser.ast.stmt.BlockStmt;
import com.github.javaparser.ast.stmt.ExpressionStmt;
import com.github.javaparser.ast.stmt.Statement;
import com.github.javaparser.ast.stmt.ThrowStmt;

import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.nio.file.*;
import java.util.*;
import java.util.stream.Collectors;
import java.util.stream.Stream;

public class JavaAdapter {
    static final Path ROOT = Paths.get("").toAbsolutePath();
    static final Path MAIN = ROOT.resolve("src/main/java");
    static final Path TEST = ROOT.resolve("src/test/java");
    static final Path LIB = Paths.get(System.getProperty("graybox.lib", "lib")).toAbsolutePath();

    public static void main(String[] args) throws Exception {
        StaticJavaParser.setConfiguration(new ParserConfiguration().setLanguageLevel(ParserConfiguration.LanguageLevel.JAVA_21));
        String op = args.length > 0 ? args[args.length - 1] : "";
        Map<String, String> in = Json.parse(new String(System.in.readAllBytes(), StandardCharsets.UTF_8));
        Object out;
        try {
            out = switch (op) {
                case "resolve" -> resolve(in.get("symbol"));
                case "apply" -> apply(in.get("symbol"), in.get("body"));
                case "checks" -> Map.of("checks", checks());
                case "preconditions" -> Map.of("checks", List.of());
                case "list_tests" -> Map.of("tests", listTests());
                case "stubs" -> Map.of("stubs", stubs());
                case "api_snapshot" -> Map.of("lines", apiSnapshot());
                default -> Map.of("error", "usage: JavaAdapter resolve|apply|checks|preconditions|list_tests|api_snapshot");
            };
        } catch (LookupException e) {
            out = Map.of("error", e.getMessage());
        }
        System.out.println(Json.write(out));
    }

    static class LookupException extends Exception {
        LookupException(String m) { super(m); }
    }

    // ---------- symbols ----------

    record Found(Path file, String text, CompilationUnit cu, CallableDeclaration<?> method, BlockStmt body) {}

    static Found find(String symbol) throws Exception {
        int hash = symbol.indexOf('#');
        if (hash <= 0) throw new LookupException("bad Java symbol " + symbol + ": want com.pkg.Type#method");
        String fqn = symbol.substring(0, hash), name = symbol.substring(hash + 1);
        String[] parts = fqn.split("\\.");
        // Top-level type file; nested types are named Outer.Inner and searched inside.
        for (int i = parts.length; i > 0; i--) {
            String pathName = String.join("/", Arrays.copyOfRange(parts, 0, i)) + ".java";
            for (Path base : List.of(MAIN, TEST)) {
                Path f = base.resolve(pathName);
                if (!Files.isRegularFile(f)) continue;
                String text = Files.readString(f);
                CompilationUnit cu = StaticJavaParser.parse(text);
                List<String> typePath = Arrays.asList(parts).subList(i - 1, parts.length);
                TypeDeclaration<?> type = null;
                List<? extends BodyDeclaration<?>> scope = cu.getTypes();
                for (String tn : typePath) {
                    type = null;
                    for (var d : scope) if (d instanceof TypeDeclaration<?> t && t.getNameAsString().equals(tn)) type = t;
                    if (type == null) break;
                    scope = type.getMembers();
                }
                if (type == null) continue;
                if (name.equals("<init>")) {
                    var cs = type.getConstructors();
                    if (cs.size() > 1) throw new LookupException(symbol + " has several constructors; overloads are not supported");
                    if (cs.size() == 1) return new Found(f, text, cu, cs.get(0), cs.get(0).getBody());
                    continue;
                }
                List<MethodDeclaration> ms = type.getMethodsByName(name);
                if (ms.size() > 1) throw new LookupException(symbol + " is overloaded; overloads are not supported");
                if (ms.size() == 1) {
                    MethodDeclaration m = ms.get(0);
                    if (m.getBody().isEmpty()) throw new LookupException(symbol + " has no body");
                    return new Found(f, text, cu, m, m.getBody().get());
                }
            }
        }
        throw new LookupException("symbol " + symbol + " not found");
    }

    static String signature(CallableDeclaration<?> m) {
        return norm(m.getDeclarationAsString(true, true, true));
    }

    static boolean isStub(BlockStmt b) {
        if (b.getStatements().size() != 1) return false;
        Statement s = b.getStatement(0);
        if (s instanceof ThrowStmt t && t.getExpression() instanceof ObjectCreationExpr oc)
            return oc.getType().getNameAsString().equals("UnsupportedOperationException");
        if (s instanceof ExpressionStmt e && e.getExpression() instanceof MethodCallExpr mc)
            return mc.getNameAsString().equals("abort");
        return false;
    }

    static Map<String, Object> resolve(String symbol) throws Exception {
        Found f = find(symbol);
        Map<String, Object> s = new LinkedHashMap<>();
        s.put("name", symbol);
        s.put("file", ROOT.relativize(f.file).toString());
        s.put("is_stub", isStub(f.body));
        s.put("signature", signature(f.method));
        return Map.of("symbol", s);
    }

    static int offset(String text, int line, int col) {
        int off = 0;
        for (int l = 1; l < line; l++) off = text.indexOf('\n', off) + 1;
        return off + col - 1;
    }

    static Map<String, Object> apply(String symbol, String body) throws Exception {
        try {
            StaticJavaParser.parseBlock("{\n" + body + "\n}");
        } catch (Exception e) {
            return Map.of("error", "body does not parse: " + firstLine(e.getMessage()));
        }
        Found f = find(symbol);
        var r = f.body.getRange().orElseThrow();
        int open = offset(f.text, r.begin.line, r.begin.column);
        int close = offset(f.text, r.end.line, r.end.column);
        int lineStart = f.text.lastIndexOf('\n', offset(f.text, f.method.getRange().get().begin.line, 1)) + 1;
        String declIndent = f.text.substring(lineStart).replaceAll("(?s)^([ \t]*).*", "$1");
        String inner = declIndent + "    ";
        String next = f.text.substring(0, open + 1) + "\n" + indent(body, inner) + "\n" + declIndent + f.text.substring(close);
        Files.writeString(f.file, next);
        Found after = find(symbol);
        if (!signature(after.method).equals(signature(f.method))) {
            Files.writeString(f.file, f.text);
            return Map.of("error", "apply would change the signature");
        }
        return Map.of();
    }

    static String indent(String body, String inner) {
        List<String> lines = Arrays.asList(body.replaceAll("^\n+|\n+$", "").split("\n", -1));
        int min = lines.stream().filter(l -> !l.isBlank()).mapToInt(l -> l.length() - l.stripLeading().length()).min().orElse(0);
        return lines.stream().map(l -> l.isBlank() ? "" : inner + l.substring(min)).collect(Collectors.joining("\n"));
    }

    static List<String> stubs() throws Exception {
        List<String> out = new ArrayList<>();
        for (Path f : concat(javaFiles(MAIN), javaFiles(TEST))) {
            CompilationUnit cu = StaticJavaParser.parse(f);
            String pkg = cu.getPackageDeclaration().map(p -> p.getNameAsString() + ".").orElse("");
            for (TypeDeclaration<?> t : cu.getTypes()) stubsIn(pkg + t.getNameAsString(), t, out);
        }
        Collections.sort(out);
        return out;
    }

    static void stubsIn(String fq, TypeDeclaration<?> t, List<String> out) {
        for (BodyDeclaration<?> m : t.getMembers()) {
            if (m instanceof MethodDeclaration md && md.getBody().isPresent() && isStub(md.getBody().get()))
                out.add(fq + "#" + md.getNameAsString());
            else if (m instanceof ConstructorDeclaration cd && isStub(cd.getBody()))
                out.add(fq + "#<init>");
            else if (m instanceof TypeDeclaration<?> nt)
                stubsIn(fq + "." + nt.getNameAsString(), nt, out);
        }
    }

    // ---------- files ----------

    static List<Path> javaFiles(Path base) throws IOException {
        if (!Files.isDirectory(base)) return List.of();
        try (Stream<Path> s = Files.walk(base)) {
            return s.filter(p -> p.toString().endsWith(".java")).sorted().toList();
        }
    }

    // ---------- checks ----------

    record Run(int code, String out) {}

    static Run run(List<String> cmd) throws Exception {
        Process p = new ProcessBuilder(cmd).directory(ROOT.toFile()).redirectErrorStream(true).start();
        String out = new String(p.getInputStream().readAllBytes(), StandardCharsets.UTF_8).strip();
        return new Run(p.waitFor(), out);
    }

    static Map<String, String> check(String name, String status, String detail) {
        Map<String, String> m = new LinkedHashMap<>();
        m.put("name", name);
        m.put("status", status);
        m.put("detail", detail);
        return m;
    }

    static String bin(String tool) {
        return Paths.get(System.getProperty("java.home"), "bin", tool).toString();
    }

    static List<Map<String, String>> checks() throws Exception {
        List<Map<String, String>> cs = new ArrayList<>();
        List<String> bad = new ArrayList<>();
        for (Path f : concat(javaFiles(MAIN), javaFiles(TEST))) {
            try {
                StaticJavaParser.parse(f);
            } catch (Exception e) {
                bad.add(ROOT.relativize(f) + ": " + firstLine(e.getMessage()));
            }
        }
        cs.add(check("syntax", bad.isEmpty() ? "pass" : "fail", String.join("\n", bad)));
        if (!bad.isEmpty()) return cs;

        Path build = ROOT.resolve("build/graybox");
        deleteTree(build);
        Path mainOut = build.resolve("main"), testOut = build.resolve("test");
        String junit = junitJar();
        // Extra libraries a project needs (e.g. org.json) come from GRAYBOX_JAVA_CP.
        String extra = Optional.ofNullable(System.getenv("GRAYBOX_JAVA_CP")).orElse("");
        String extraCp = extra.isEmpty() ? "" : ":" + extra;
        List<String> cmd = new ArrayList<>(List.of(bin("javac"), "-Xlint:all", "-d", mainOut.toString(), "-cp", mainOut + extraCp));
        javaFiles(MAIN).forEach(p -> cmd.add(p.toString()));
        Run r = run(cmd);
        if (r.code == 0 && !javaFiles(TEST).isEmpty()) {
            List<String> tcmd = new ArrayList<>(List.of(bin("javac"), "-d", testOut.toString(), "-cp", mainOut + ":" + junit + extraCp));
            javaFiles(TEST).forEach(p -> tcmd.add(p.toString()));
            r = run(tcmd);
        }
        cs.add(check("build", r.code == 0 ? "pass" : "fail", r.code == 0 ? "" : tail(r.out)));
        if (r.code != 0) {
            cs.add(check("test", "skipped", "build failed"));
            return cs;
        }
        cs.add(check("lint", "skipped", "Checkstyle not configured"));
        Run t = run(List.of(bin("java"), "-jar", junit, "execute", "--class-path", mainOut + ":" + testOut + extraCp,
                "--scan-class-path", "--disable-banner", "--details=summary", "--fail-if-no-tests"));
        cs.add(check("test", t.code == 0 ? "pass" : "fail", t.code == 0 ? "" : tail(t.out)));
        return cs;
    }

    static String junitJar() throws IOException {
        try (Stream<Path> s = Files.list(LIB)) {
            return s.filter(p -> p.getFileName().toString().startsWith("junit-platform-console-standalone"))
                    .findFirst().map(Path::toString).orElseThrow(() -> new IOException("junit console jar missing in " + LIB));
        }
    }

    // ---------- tests and API ----------

    static List<String> listTests() throws Exception {
        List<String> names = new ArrayList<>();
        for (Path f : javaFiles(TEST)) {
            for (MethodDeclaration m : StaticJavaParser.parse(f).findAll(MethodDeclaration.class))
                if (m.getAnnotationByName("Test").isPresent()) names.add(m.getNameAsString());
        }
        Collections.sort(names);
        return names;
    }

    static boolean isPublic(NodeWithModifiersLike n, boolean inInterface) {
        return inInterface || n.has(Modifier.Keyword.PUBLIC);
    }

    interface NodeWithModifiersLike { boolean has(Modifier.Keyword k); }

    static NodeWithModifiersLike mods(com.github.javaparser.ast.nodeTypes.NodeWithModifiers<?> n) {
        return k -> n.hasModifier(k);
    }

    static List<String> apiSnapshot() throws Exception {
        List<String> lines = new ArrayList<>();
        for (Path f : javaFiles(MAIN)) {
            CompilationUnit cu = StaticJavaParser.parse(f);
            String pkg = cu.getPackageDeclaration().map(p -> p.getNameAsString() + ".").orElse("");
            for (TypeDeclaration<?> t : cu.getTypes()) snapshotType(pkg, t, lines);
        }
        Collections.sort(lines);
        return lines;
    }

    static void snapshotType(String prefix, TypeDeclaration<?> t, List<String> lines) {
        if (!t.hasModifier(Modifier.Keyword.PUBLIC)) return;
        String fq = prefix + t.getNameAsString();
        lines.add(fq + ": " + header(t));
        boolean iface = t instanceof ClassOrInterfaceDeclaration c && c.isInterface();
        if (t instanceof EnumDeclaration e)
            for (EnumConstantDeclaration c : e.getEntries()) lines.add(fq + ": enum constant " + norm(c.toString()));
        for (BodyDeclaration<?> m : t.getMembers()) {
            if (m instanceof MethodDeclaration md && isPublic(mods(md), iface))
                lines.add(fq + ": " + signature(md));
            else if (m instanceof ConstructorDeclaration cd && cd.hasModifier(Modifier.Keyword.PUBLIC))
                lines.add(fq + ": " + norm(cd.getDeclarationAsString(true, true, true)));
            else if (m instanceof FieldDeclaration fd && isPublic(mods(fd), iface))
                lines.add(fq + ": " + norm(fd.toString()));
            else if (m instanceof TypeDeclaration<?> nt)
                snapshotType(fq + ".", nt, lines);
        }
    }

    static String header(TypeDeclaration<?> t) {
        String s = t.toString();
        int brace = s.indexOf('{');
        String head = brace >= 0 ? s.substring(0, brace) : s;
        // drop javadoc and annotations-only lines are kept: they are part of the contract
        return norm(head.replaceAll("(?s)/\\*\\*.*?\\*/", ""));
    }

    // ---------- util ----------

    static <T> List<T> concat(List<T> a, List<T> b) {
        List<T> r = new ArrayList<>(a);
        r.addAll(b);
        return r;
    }

    static void deleteTree(Path p) throws IOException {
        if (!Files.exists(p)) return;
        try (Stream<Path> s = Files.walk(p)) {
            for (Path x : s.sorted(Comparator.reverseOrder()).toList()) Files.delete(x);
        }
    }

    static String norm(String s) { return s.replaceAll("\\s+", " ").strip(); }

    static String firstLine(String s) { return s == null ? "" : s.split("\n", 2)[0]; }

    static String tail(String s) { return s.length() <= 2000 ? s : "..." + s.substring(s.length() - 2000); }

    // Minimal JSON for this protocol: input is a flat object of strings; output
    // is maps, lists, strings and booleans.
    static final class Json {
        static Map<String, String> parse(String s) {
            Map<String, String> m = new HashMap<>();
            int[] i = {skip(s, 0)};
            if (i[0] >= s.length() || s.charAt(i[0]) != '{') return m;
            i[0] = skip(s, i[0] + 1);
            while (i[0] < s.length() && s.charAt(i[0]) != '}') {
                String k = str(s, i);
                i[0] = skip(s, i[0]);
                i[0] = skip(s, i[0] + 1); // ':'
                String v = str(s, i);
                m.put(k, v);
                i[0] = skip(s, i[0]);
                if (s.charAt(i[0]) == ',') i[0] = skip(s, i[0] + 1);
            }
            return m;
        }

        static int skip(String s, int i) {
            while (i < s.length() && Character.isWhitespace(s.charAt(i))) i++;
            return i;
        }

        static String str(String s, int[] i) {
            StringBuilder b = new StringBuilder();
            int j = i[0] + 1;
            while (s.charAt(j) != '"') {
                char c = s.charAt(j++);
                if (c == '\\') {
                    char e = s.charAt(j++);
                    switch (e) {
                        case 'n' -> b.append('\n');
                        case 't' -> b.append('\t');
                        case 'r' -> b.append('\r');
                        case 'b' -> b.append('\b');
                        case 'f' -> b.append('\f');
                        case 'u' -> { b.append((char) Integer.parseInt(s.substring(j, j + 4), 16)); j += 4; }
                        default -> b.append(e);
                    }
                } else b.append(c);
            }
            i[0] = j + 1;
            return b.toString();
        }

        static String write(Object o) {
            if (o == null) return "null";
            if (o instanceof String s) {
                StringBuilder b = new StringBuilder("\"");
                for (char c : s.toCharArray()) {
                    switch (c) {
                        case '"' -> b.append("\\\"");
                        case '\\' -> b.append("\\\\");
                        case '\n' -> b.append("\\n");
                        case '\r' -> b.append("\\r");
                        case '\t' -> b.append("\\t");
                        default -> { if (c < 0x20) b.append(String.format("\\u%04x", (int) c)); else b.append(c); }
                    }
                }
                return b.append('"').toString();
            }
            if (o instanceof Boolean || o instanceof Number) return o.toString();
            if (o instanceof Map<?, ?> m)
                return m.entrySet().stream().map(e -> write(e.getKey().toString()) + ":" + write(e.getValue()))
                        .collect(Collectors.joining(",", "{", "}"));
            if (o instanceof Collection<?> c) return c.stream().map(Json::write).collect(Collectors.joining(",", "[", "]"));
            return write(o.toString());
        }
    }
}
