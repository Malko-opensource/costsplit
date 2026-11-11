// Kotlin language adapter for graybox, built on the Kotlin compiler's PSI.
//
// Run through kotlin-adapter.sh (cwd = project root). Reads one JSON object on
// stdin, prints one JSON object.
//
@file:OptIn(org.jetbrains.kotlin.config.CompilerConfiguration.Internals::class, org.jetbrains.kotlin.compiler.plugin.ExperimentalCompilerApi::class)

// Layout: src/main/kotlin, src/test/kotlin (JUnit 6). Symbols look like
// "com.acme.order.Service#apply". Stubs are `TODO()`; skipped tests call
// `Assumptions.abort(...)`.
import org.jetbrains.kotlin.cli.jvm.compiler.EnvironmentConfigFiles
import com.intellij.openapi.project.Project
import com.intellij.openapi.util.Disposer
import com.intellij.psi.PsiErrorElement
import org.jetbrains.kotlin.config.CommonConfigurationKeys
import org.jetbrains.kotlin.config.CompilerConfiguration
import org.jetbrains.kotlin.lexer.KtTokens
import org.jetbrains.kotlin.psi.*
import org.jetbrains.kotlin.psi.psiUtil.collectDescendantsOfType
import org.jetbrains.kotlin.psi.psiUtil.startOffset
import java.io.File
import java.nio.file.Files
import java.nio.file.Path
import java.nio.file.Paths
import kotlin.io.path.*
import kotlin.system.exitProcess

val ROOT: Path = Paths.get("").toAbsolutePath()
val MAIN: Path = ROOT.resolve("src/main/kotlin")
val TEST: Path = ROOT.resolve("src/test/kotlin")
val LIB: Path = Paths.get(System.getProperty("graybox.lib", "lib")).toAbsolutePath()
val KOTLIN_LIB: Path = Paths.get(System.getProperty("graybox.kotlinLib", "")).toAbsolutePath()

class LookupException(msg: String) : Exception(msg)

lateinit var project: Project

fun main(args: Array<String>) {
    val config = CompilerConfiguration()
    config.put(CommonConfigurationKeys.MODULE_NAME, "graybox-adapter")
    Class.forName("org.jetbrains.kotlin.cli.FrontendConfigurationKeysKt")
        .getMethod("setExtensionsStorage", CompilerConfiguration::class.java,
            org.jetbrains.kotlin.compiler.plugin.CompilerPluginRegistrar.ExtensionStorage::class.java)
        .invoke(null, config, org.jetbrains.kotlin.compiler.plugin.CompilerPluginRegistrar.ExtensionStorage())
    // KotlinCoreEnvironment needs a compile-time opt-in whose marker is not
    // public; calling it reflectively keeps the adapter buildable.
    val cls = Class.forName("org.jetbrains.kotlin.cli.jvm.compiler.KotlinCoreEnvironment")
    val companion = cls.getField("Companion").get(null)
    val create = companion.javaClass.getMethod(
        "createForProduction", com.intellij.openapi.Disposable::class.java,
        CompilerConfiguration::class.java, EnvironmentConfigFiles::class.java)
    val env = create.invoke(companion, Disposer.newDisposable(), config, EnvironmentConfigFiles.JVM_CONFIG_FILES)
    project = cls.getMethod("getProject").invoke(env) as Project

    val input = Json.parse(System.`in`.readBytes().toString(Charsets.UTF_8))
    val out: Any = try {
        when (args.lastOrNull()) {
            "resolve" -> resolve(input.getValue("symbol"))
            "apply" -> apply(input.getValue("symbol"), input.getValue("body"))
            "checks" -> mapOf("checks" to checks())
            "preconditions" -> mapOf("checks" to emptyList<Any>())
            "list_tests" -> mapOf("tests" to listTests())
            "stubs" -> mapOf("stubs" to stubs())
            "api_snapshot" -> mapOf("lines" to apiSnapshot())
            else -> mapOf("error" to "usage: KotlinAdapter resolve|apply|checks|preconditions|list_tests|api_snapshot")
        }
    } catch (e: LookupException) {
        mapOf("error" to (e.message ?: "lookup failed"))
    }
    println(Json.write(out))
    exitProcess(0)
}

fun parse(path: Path, text: String = path.readText()): KtFile =
    KtPsiFactory(project, false).createFile(path.fileName.toString(), text)

fun ktFiles(base: Path): List<Path> =
    if (!base.isDirectory()) emptyList()
    else Files.walk(base).use { s -> s.filter { it.toString().endsWith(".kt") }.sorted().toList() }

fun syntaxErrors(f: KtFile): List<String> =
    f.collectDescendantsOfType<PsiErrorElement>().map { "${it.errorDescription} at offset ${it.textRange.startOffset}" }

// ---------- symbols ----------

data class Found(val path: Path, val text: String, val file: KtFile, val fn: KtNamedFunction)

fun find(symbol: String): Found {
    val hash = symbol.indexOf('#')
    if (hash <= 0) throw LookupException("bad Kotlin symbol $symbol: want com.pkg.Type#function")
    val fqn = symbol.substring(0, hash)
    val name = symbol.substring(hash + 1)
    for (path in ktFiles(MAIN) + ktFiles(TEST)) {
        val text = path.readText()
        val file = parse(path, text)
        val pkg = file.packageFqName.asString()
        val prefix = if (pkg.isEmpty()) "" else "$pkg."
        if (!fqn.startsWith(prefix)) continue
        val typePath = fqn.removePrefix(prefix).split('.')
        var scope: List<KtDeclaration> = file.declarations
        var ok = true
        for (tn in typePath) {
            val c = scope.filterIsInstance<KtClassOrObject>().firstOrNull { it.name == tn }
            if (c == null) { ok = false; break }
            scope = c.declarations
        }
        if (!ok) continue
        val fns = scope.filterIsInstance<KtNamedFunction>().filter { it.name == name }
        if (fns.size > 1) throw LookupException("$symbol is overloaded; overloads are not supported")
        if (fns.size == 1) return Found(path, text, file, fns[0])
    }
    throw LookupException("symbol $symbol not found")
}

fun signature(fn: KtNamedFunction): String {
    val start = (fn.modifierList ?: fn.funKeyword ?: fn).startOffset
    val end = fn.equalsToken?.startOffset ?: fn.bodyBlockExpression?.startOffset ?: fn.textRange.endOffset
    return norm(fn.containingFile.text.substring(start, end))
}

fun isStubCall(e: KtExpression?): Boolean {
    val call = when (e) {
        is KtCallExpression -> e
        is KtDotQualifiedExpression -> e.selectorExpression as? KtCallExpression
        else -> null
    } ?: return false
    val n = call.calleeExpression?.text
    return n == "TODO" || n == "abort"
}

fun isStub(fn: KtNamedFunction): Boolean {
    val block = fn.bodyBlockExpression
    if (block != null) return block.statements.size == 1 && isStubCall(block.statements[0])
    return isStubCall(fn.bodyExpression)
}

fun resolve(symbol: String): Map<String, Any> {
    val f = find(symbol)
    return mapOf("symbol" to linkedMapOf(
        "name" to symbol, "file" to ROOT.relativize(f.path).toString(),
        "is_stub" to isStub(f.fn), "signature" to signature(f.fn)))
}

fun indent(body: String, inner: String): String {
    val lines = body.trim('\n').split("\n")
    val min = lines.filter { it.isNotBlank() }.minOfOrNull { it.length - it.trimStart().length } ?: 0
    return lines.joinToString("\n") { if (it.isBlank()) "" else inner + it.substring(min) }
}

fun apply(symbol: String, body: String): Map<String, Any> {
    val f = find(symbol)
    val text = f.text
    val fnStart = f.fn.startOffset
    val lineStart = text.lastIndexOf('\n', fnStart - 1) + 1
    val declIndent = text.substring(lineStart).takeWhile { it == ' ' || it == '\t' }
    val inner = "$declIndent    "
    val block = f.fn.bodyBlockExpression
    val next = if (block != null) {
        val open = block.lBrace!!.startOffset
        val close = block.rBrace!!.startOffset
        text.substring(0, open + 1) + "\n" + indent(body, inner) + "\n" + declIndent + text.substring(close)
    } else {
        val eq = f.fn.equalsToken ?: throw LookupException("$symbol has no body")
        val end = f.fn.textRange.endOffset
        text.substring(0, eq.startOffset).trimEnd() + " {\n" + indent(body, inner) + "\n" + declIndent + "}" + text.substring(end)
    }
    val errs = syntaxErrors(parse(f.path, next))
    if (errs.isNotEmpty()) return mapOf("error" to "body does not parse: ${errs[0]}")
    f.path.writeText(next)
    val after = find(symbol)
    if (signature(after.fn) != signature(f.fn)) {
        f.path.writeText(text)
        return mapOf("error" to "apply would change the signature")
    }
    return emptyMap()
}

fun stubs(): List<String> {
    val out = mutableListOf<String>()
    fun walk(prefix: String, decls: List<KtDeclaration>) {
        for (d in decls) when (d) {
            is KtClassOrObject -> walk("$prefix${d.name}.", d.declarations)
            is KtNamedFunction -> if (isStub(d)) out += prefix.trimEnd('.') + "#" + d.name
            else -> {}
        }
    }
    for (p in ktFiles(MAIN) + ktFiles(TEST)) {
        val f = parse(p)
        val pkg = f.packageFqName.asString().let { if (it.isEmpty()) "" else "$it." }
        walk(pkg, f.declarations)
    }
    return out.sorted()
}

// ---------- checks ----------

data class Run(val code: Int, val out: String)

fun run(cmd: List<String>): Run {
    val pb = ProcessBuilder(cmd).directory(ROOT.toFile()).redirectErrorStream(true)
    pb.environment()["JAVA_HOME"] = System.getProperty("java.home")
    val p = pb.start()
    val out = p.inputStream.readBytes().toString(Charsets.UTF_8).trim()
    return Run(p.waitFor(), out)
}

fun check(name: String, status: String, detail: String = "") = linkedMapOf("name" to name, "status" to status, "detail" to detail)

fun tail(s: String) = if (s.length <= 2000) s else "..." + s.takeLast(2000)

fun junitJar(): String = Files.list(LIB).use { s ->
    s.map { it.toString() }.filter { File(it).name.startsWith("junit-platform-console-standalone") }.findFirst().orElseThrow()
}

fun checks(): List<Map<String, String>> {
    val cs = mutableListOf<Map<String, String>>()
    val bad = (ktFiles(MAIN) + ktFiles(TEST)).flatMap { p -> syntaxErrors(parse(p)).map { "${ROOT.relativize(p)}: $it" } }
    cs += check("syntax", if (bad.isEmpty()) "pass" else "fail", bad.joinToString("\n"))
    if (bad.isNotEmpty()) return cs

    val kotlinc = KOTLIN_LIB.resolve("../bin/kotlinc").normalize().toString()
    val stdlib = KOTLIN_LIB.resolve("kotlin-stdlib.jar").toString()
    val build = ROOT.resolve("build/graybox")
    build.toFile().deleteRecursively()
    val mainOut = build.resolve("main").toString()
    val testOut = build.resolve("test").toString()
    val junit = junitJar()
    var r = run(listOf(kotlinc, "-Werror", "-nowarn", "-d", mainOut) + ktFiles(MAIN).map { it.toString() })
    if (r.code == 0 && ktFiles(TEST).isNotEmpty())
        r = run(listOf(kotlinc, "-d", testOut, "-cp", "$mainOut:$junit") + ktFiles(TEST).map { it.toString() })
    cs += check("build", if (r.code == 0) "pass" else "fail", if (r.code == 0) "" else tail(r.out))
    if (r.code != 0) {
        cs += check("test", "skipped", "build failed")
        return cs
    }
    cs += check("lint", "skipped", "ktlint/detekt not configured")
    val java = Paths.get(System.getProperty("java.home"), "bin", "java").toString()
    val t = run(listOf(java, "-jar", junit, "execute", "--class-path", "$mainOut:$testOut:$stdlib",
        "--scan-class-path", "--disable-banner", "--details=summary", "--fail-if-no-tests"))
    cs += check("test", if (t.code == 0) "pass" else "fail", if (t.code == 0) "" else tail(t.out))
    return cs
}

// ---------- tests and API ----------

fun listTests(): List<String> = ktFiles(TEST).flatMap { p ->
    parse(p).collectDescendantsOfType<KtNamedFunction>()
        .filter { fn -> fn.annotationEntries.any { it.shortName?.asString() == "Test" } }
        .mapNotNull { it.name }
}.sorted()

fun isPublic(d: KtModifierListOwner): Boolean =
    !d.hasModifier(KtTokens.PRIVATE_KEYWORD) && !d.hasModifier(KtTokens.INTERNAL_KEYWORD) && !d.hasModifier(KtTokens.PROTECTED_KEYWORD)

fun apiSnapshot(): List<String> {
    val lines = mutableListOf<String>()
    for (p in ktFiles(MAIN)) {
        val file = parse(p)
        val pkg = file.packageFqName.asString().let { if (it.isEmpty()) "" else "$it." }
        snapshot(pkg, file.declarations, lines)
    }
    return lines.sorted()
}

fun snapshot(prefix: String, decls: List<KtDeclaration>, lines: MutableList<String>) {
    for (d in decls) {
        if (!isPublic(d)) continue
        when (d) {
            is KtClassOrObject -> {
                val fq = prefix + (d.name ?: "companion")
                val head = d.body?.let { d.text.substring(0, it.startOffsetInParent) } ?: d.text
                lines += "$fq: " + norm(head.replace(Regex("(?s)/\\*\\*.*?\\*/"), ""))
                if (d is KtClass && d.isEnum())
                    d.declarations.filterIsInstance<KtEnumEntry>().forEach { lines += "$fq: enum entry ${it.name}" }
                snapshot("$fq.", d.declarations.filter { it !is KtEnumEntry }, lines)
            }
            is KtNamedFunction -> lines += prefix.trimEnd('.') + ": " + signature(d)
            is KtProperty -> lines += prefix.trimEnd('.') + ": " + norm(d.text.replace(Regex("(?s)/\\*\\*.*?\\*/"), ""))
            else -> {}
        }
    }
}

fun norm(s: String) = s.replace(Regex("\\s+"), " ").trim()

// Minimal JSON: input is a flat object of strings; output is maps, lists,
// strings and booleans.
object Json {
    fun parse(s: String): Map<String, String> {
        val m = HashMap<String, String>()
        var i = skip(s, 0)
        if (i >= s.length || s[i] != '{') return m
        i = skip(s, i + 1)
        while (i < s.length && s[i] != '}') {
            val (k, a) = str(s, i)
            i = skip(s, skip(s, a) + 1)
            val (v, b) = str(s, i)
            m[k] = v
            i = skip(s, b)
            if (i < s.length && s[i] == ',') i = skip(s, i + 1)
        }
        return m
    }

    private fun skip(s: String, start: Int): Int {
        var i = start
        while (i < s.length && s[i].isWhitespace()) i++
        return i
    }

    private fun str(s: String, start: Int): Pair<String, Int> {
        val b = StringBuilder()
        var j = start + 1
        while (s[j] != '"') {
            val c = s[j++]
            if (c == '\\') {
                when (val e = s[j++]) {
                    'n' -> b.append('\n'); 't' -> b.append('\t'); 'r' -> b.append('\r')
                    'b' -> b.append('\b'); 'f' -> b.append('\u000c')
                    'u' -> { b.append(s.substring(j, j + 4).toInt(16).toChar()); j += 4 }
                    else -> b.append(e)
                }
            } else b.append(c)
        }
        return b.toString() to j + 1
    }

    fun write(o: Any?): String = when (o) {
        null -> "null"
        is String -> buildString {
            append('"')
            for (c in o) when (c) {
                '"' -> append("\\\""); '\\' -> append("\\\\"); '\n' -> append("\\n")
                '\r' -> append("\\r"); '\t' -> append("\\t")
                else -> if (c < ' ') append("\\u%04x".format(c.code)) else append(c)
            }
            append('"')
        }
        is Boolean, is Number -> o.toString()
        is Map<*, *> -> o.entries.joinToString(",", "{", "}") { write(it.key.toString()) + ":" + write(it.value) }
        is Collection<*> -> o.joinToString(",", "[", "]") { write(it) }
        else -> write(o.toString())
    }
}
