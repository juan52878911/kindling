package triage

import (
	"regexp"
	"strings"
)

// Categorías de fallo. "flaky" no sale nunca de un solo log (hace falta saber
// que el mismo commit pasó al repetirlo): solo la pone una persona al
// confirmar, y así entra en los datos para reentrenar.
const (
	CatTest       = "test"       // un test o una aserción falló
	CatBuild      = "build"      // no compila: sintaxis, tipos, enlazado, CMake…
	CatLint       = "lint"       // estilo, formato, análisis estático, comprobaciones de docs
	CatDependency = "dependency" // instalar o resolver dependencias
	CatConfig     = "config"     // el propio CI: guion o fichero que falta, permisos, opciones, secretos
	CatInfra      = "infra"      // red, servicios externos, Docker, puertos
	CatTimeout    = "timeout"    // el trabajo o la suite superó su tiempo
	CatResources  = "resources"  // memoria o disco
	CatOther      = "other"
	CatFlaky      = "flaky" // solo humano
)

// Categories son las que puede predecir el modelo (sin flaky).
var Categories = []string{CatTest, CatBuild, CatLint, CatDependency, CatConfig, CatInfra, CatTimeout, CatResources, CatOther}

// HumanCategories son las que acepta la confirmación de una persona.
var HumanCategories = append(append([]string(nil), Categories...), CatFlaky)

// ValidHumanCategory dice si c es una categoría que una persona puede poner.
func ValidHumanCategory(c string) bool {
	for _, x := range HumanCategories {
		if c == x {
			return true
		}
	}
	return false
}

type rule struct {
	cat string
	re  *regexp.Regexp
}

// categoryRules son las reglas de etiquetado débil y, a la vez, la línea base
// «regex» de la categoría. Se escribieron mirando SOLO los trozos de
// entrenamiento de LogChunks; el orden importa (gana la primera): lo más
// específico (memoria, tiempo, dependencias, red) antes que lo genérico (un
// test que falla, un error de compilación).
var categoryRules = []rule{
	{CatResources, re(`out of memory|oom[- ]?kill|cannot allocate memory|enomem|heap out of memory|no space left on device|killed signal 9|exit(ed with| code) 137|memoryerror`)},
	{CatTimeout, re(`no output has been received in the last|exceeded the maximum (execution )?time|tests timed out|exited with 124|process timed out|job was cancelled because|has exceeded the maximum|the operation was canceled`)},
	{CatDependency, re(`could not find gem|could not resolve dependenc|unable to resolve dependenc|no matching distribution|packagesnotfounderror|package required but not available|requested package .{0,80} exists as|requirements could not be resolved|npm err! (code )?(e404|eresolve|404|etarget)|could not find a version that satisfies|modulenotfounderror|no module named|error installing|occur+ed while installing|installation failed|bundler could not find|could not find .{0,40}\(missing|unable to find the requested .{0,20}librar|failed to download|could not fetch|unable to locate package|dependency resolution|version solving failed|conflicting dependencies`)},
	{CatInfra, re(`connection (refused|reset|timed out)|could not resolve host|temporary failure in name resolution|network is unreachable|couldn't establish http connection|service unavailable|bad gateway|rate limit|error response from daemon|toomanyrequests|handshake|econnreset|etimedout|eai_again|address already in use|ssh: connect to host|empty reply from server|unable to access 'http|host key verification failed|i/o timeout`)},
	{CatLint, re(`rubocop|eslint|prettier|tslint|jshint|jscs|standard:|flake8|pylint|pycodestyle|pep8|gofmt|goimports|golint|gometalinter|staticcheck|clang-format|checkstyle|swiftlint|ktlint|violation|offenses?\b|\blint|line longer than|\b[ew]\d{3} |please commit all changes|> links|\b30[12] http|phpstan|psalm|-{6} -{20,}|check_integrity|is required for|code style|check-formatted|not formatted|\bmd\d{3}/|tldr\d{3}|whitespace|include guard|overfull \\hbox|size limit has exceeded|undocumented|locale dependent|indent_char|\bstyle/|defined but never used|no-unused-vars|\((quotes|semi|indent|comma-dangle)\)|words not in order|does not match format|duplicate links|which does not exist|^diff --git| is not formatted|would reformat|trailing space|mypy|type-check`)},
	{CatTest, re(`failure/error|assertionerror|assertion (with .{0,6} )?failed|assert\w* ?failed|expected:? .{0,120}(got|but (was|got)|actual|to equal|received)|--- fail|^fail\b|\bfail:|tests? failed|failing tests?|\bfailures?:|failed asserting|comparisonfailure|\[fail\]|xctassert|dubious, test returned|failed \d+/\d+ subtests|\(exunit|test suite failed|failed on line \d+|not ok \d|\d+\) test|test_\w+ _{3}|_{5} test|\btest\w* \(.*\) time elapsed|^\s*\d+\) |\[-\] |missing \d+ expected call|expect\(received\)|● |test failed|testcase|unit tests failed|reason: |risky tests|addresssanitizer|segmentation fault|\s(fail|error)$|error report`)},
	{CatBuild, re(`\berror\[e\d+\]|error cs\d+|error ts\d+|compileerror|compilation (failed|error)|syntaxerror|syntax error|parse error|undefined reference|cannot find symbol|not a member of|was not declared|unknown type name|cmake error|make: \*\*\*|build failed|failed to load interface|not in scope|couldn't match type|could not match type|fatal error:|invalid css|undefined variable|illegal nesting|inconsistent indentation|is unused|unused import|is deprecated|cannot find|unresolved (import|reference)|undefined in record|while (scanning|parsing)|\.\w{1,6}:\d+:\d+: warning|-werror|undefined (function|macro|type)|\.(c|cc|cpp|h|hpp|hs|go|rs|java|kt|scala|swift|ts|erl|ex|cs|coffee|m|mm)[:(]\d+[:,]\d+\)?:? ?(fatal )?error|^\[?error\] |❌ `)},
	{CatConfig, re(`no such file or directory|permission denied|command not found|not recognized as|is currently not installed|flag provided but not defined|illegal switch|unknown option|unrecognized option|remote branch .{0,80} not found|no rakefile found|access key|invalid reference format|unable to find a destination|secret|credential|unauthori[sz]ed|authentication failed|executable file not found|task never defined|invalid argument|not a valid|missing required|environment variable|is not set`)},
	{CatTest, re(`traceback|exception|panic:|segmentation fault|addresssanitizer|\berror\b|\bfailed\b`)},
}

func re(s string) *regexp.Regexp { return regexp.MustCompile(`(?m)` + s) }

// RuleCategory es la categoría que dan las reglas a un trozo (CatOther si
// ninguna casa).
func RuleCategory(chunk string) string {
	s := strings.ToLower(chunk)
	for _, r := range categoryRules {
		if r.re.MatchString(s) {
			return r.cat
		}
	}
	return CatOther
}

// errorLine es la línea base «regex» del localizador: lo que alguien buscaría
// a ojo en un log (y lo que mide el informe como punto de partida honesto).
var errorLine = regexp.MustCompile(`(?i)\berror\b|\bfail(ed|ure|ing)?\b|exception|traceback|fatal|panic|assert|\bE[A-Z]{3,}\b|timed? ?out|killed|denied|not found|cannot|could not|unable to|✗|✖|❌`)

// KeywordScore puntúa una línea para la línea base de palabras clave: 1 si
// casa, y un poco más cuanto más cerca del final (entre dos que casan, gana la
// más tardía: el fallo suele estar al final).
func KeywordScore(l Line, i, n int) float64 {
	if !errorLine.MatchString(l.Text) {
		return 0
	}
	return 0.5 + 0.5*float64(i+1)/float64(n)
}
