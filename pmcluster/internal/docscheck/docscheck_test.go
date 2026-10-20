// Package docscheck keeps this repository's prose honest about its code.
//
// Every assertion below exists because the claim it checks was false here at
// some point: a documented cluster setting the code does not define
// (`openobserve_logs_retention_days`, real key `oo_logs_retention_days`), a
// documented architecture with no implementation behind it, a link to a file
// that had been deleted, and a Mermaid block that rendered as a browser error.
//
// None of those were caught by reading carefully. They were caught by
// extracting the claim and checking it against the source — which is what this
// package does, so nobody has to catch them by hand again.
//
// The checks are deliberately exact: a gate that produces false failures gets
// disabled, and a disabled gate protects nothing. Where a check cannot be made
// exact it reports instead of failing, and says so.
package docscheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// locating the repository
// ---------------------------------------------------------------------------

// repoRoot walks up from this test file until it finds the directory holding
// the docs tree, so the checks work identically from any working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "docs", "settings.md")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("repository root not found (no directory above this file contains docs/settings.md)")
	return ""
}

// goFiles returns every non-test .go file under dir, recursively.
func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

// markdownDocs returns the in-scope prose: the root README and docs/*.md.
// CHANGELOG.md is history and test-reports record measurements taken at a
// point in time — neither is asserted against the current code.
func markdownDocs(t *testing.T, root string) []string {
	t.Helper()
	docs := []string{filepath.Join(root, "README.md"), filepath.Join(root, "agent.md")}
	matches, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatalf("glob docs: %v", err)
	}
	docs = append(docs, matches...)
	pkgDocs, err := filepath.Glob(filepath.Join(root, "pmcluster", "docs", "*.md"))
	if err != nil {
		t.Fatalf("glob pmcluster/docs: %v", err)
	}
	docs = append(docs, pkgDocs...)
	docs = append(docs, filepath.Join(root, "pmcluster", "README.md"),
		filepath.Join(root, "pmcluster", "internal", "ARCHITECTURE.md"))

	var present []string
	for _, d := range docs {
		if _, err := os.Stat(d); err == nil {
			present = append(present, d)
		}
	}
	return present
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return r
}

// ---------------------------------------------------------------------------
// 1. documented setting keys must exist in the code
// ---------------------------------------------------------------------------

var (
	reSettingConst = regexp.MustCompile(`(?m)^\s*setting[A-Za-z0-9]+\s*=\s*"([a-z0-9_]+)"`)
	reLeaderKey    = regexp.MustCompile(`(?m)^\s*(?:const\s+)?storageLeaderKey\s*=\s*"([a-z0-9_]+)"`)
)

// codeSettingKeys returns every cluster setting key the code can read or write.
func codeSettingKeys(t *testing.T, root string) map[string]string {
	t.Helper()
	found := map[string]string{}
	scan := func(pattern *regexp.Regexp, path string) {
		src := read(t, filepath.Join(root, "pmcluster", path))
		for _, m := range pattern.FindAllStringSubmatch(src, -1) {
			found[m[1]] = path
		}
	}
	scan(reSettingConst, "internal/cluster/cluster_state.go")
	scan(reLeaderKey, "internal/cluster/update.go")
	if len(found) == 0 {
		t.Fatal("parsed zero setting keys out of the code — the extraction pattern is stale, not the docs")
	}
	return found
}

// documentedKeyCells pulls the keys out of the first column of every table row
// in a settings reference. A leading underscore marks a continued shorthand
// from the previous cell (e.g. "`backup_s3_endpoint` / `_bucket`"), so those
// are skipped rather than mistaken for keys.
func documentedKeyCells(src string) []string {
	var keys []string
	for _, line := range strings.Split(src, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		first := strings.Split(strings.TrimPrefix(strings.TrimSpace(line), "|"), "|")[0]
		for _, m := range regexp.MustCompile("`([a-z][a-z0-9_]*)`").FindAllStringSubmatch(first, -1) {
			if strings.HasPrefix(m[1], "_") {
				continue
			}
			keys = append(keys, m[1])
			break // only the leading key of a cell is asserted
		}
	}
	return keys
}

func TestDocumentedSettingKeysExist(t *testing.T) {
	root := repoRoot(t)
	code := codeSettingKeys(t, root)
	target := filepath.Join(root, "docs", "settings.md")
	keys := documentedKeyCells(read(t, target))
	if len(keys) == 0 {
		t.Fatalf("parsed zero documented setting keys out of %s — the docs moved, not the extraction", rel(root, target))
	}

	for _, key := range keys {
		if _, ok := code[key]; ok {
			continue
		}
		// Per-stack pins are written directly through the store, deliberately
		// outside the settings allowlist, and are named per stack.
		if strings.HasPrefix(key, "stack_pin_") {
			continue
		}
		t.Errorf("%s documents setting %q, which no Go constant defines — an operator copying it gets the key rejected as unknown",
			rel(root, target), key)
	}
	t.Logf("checked %d documented setting keys against %d defined keys", len(keys), len(code))
}

// ---------------------------------------------------------------------------
// 2. documented CLI commands and flags must exist
// ---------------------------------------------------------------------------

var (
	reUseString = regexp.MustCompile(`Use:\s+"([^"]+)"`)
	reFlagCall  = regexp.MustCompile(`Flags\(\)\.[A-Za-z]+\(([^)]*)\)`)
	reQuoted    = regexp.MustCompile(`"([a-zA-Z0-9-]+)"`)
)

type cliSurface struct {
	commands map[string]bool
	flags    map[string]bool
}

func codeCLISurface(t *testing.T, root string) cliSurface {
	t.Helper()
	s := cliSurface{commands: map[string]bool{}, flags: map[string]bool{}}
	for _, f := range goFiles(t, filepath.Join(root, "pmcluster", "internal", "cli")) {
		src := read(t, f)
		for _, m := range reUseString.FindAllStringSubmatch(src, -1) {
			word := strings.Fields(m[1])[0]
			word = strings.Trim(word, "<>[]")
			if word != "" {
				s.commands[word] = true
			}
		}
		// The flag name is the first string argument, whether the call is
		// String("name", …) or StringVar(&target, "name", …).
		for _, m := range reFlagCall.FindAllStringSubmatch(src, -1) {
			if q := reQuoted.FindStringSubmatch(m[1]); q != nil {
				s.flags[q[1]] = true
			}
		}
	}
	if len(s.commands) == 0 || len(s.flags) == 0 {
		t.Fatal("parsed zero commands or flags out of the CLI — the extraction pattern is stale, not the docs")
	}
	return s
}

// fencedBlocks returns the contents of every ```-fenced block, plus every
// inline `code` span. Commands are only asserted inside these: prose that says
// "pmcluster translates the manifest" is a sentence, not an invocation, and
// treating it as one produces exactly the false failures that get a gate
// switched off.
func fencedBlocks(src string) []string {
	var out []string
	lines := strings.Split(src, "\n")
	inFence := false
	var buf []string
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			if inFence {
				out = append(out, strings.Join(buf, "\n"))
				buf = nil
			}
			inFence = !inFence
			continue
		}
		if inFence {
			buf = append(buf, l)
		}
	}
	for _, m := range regexp.MustCompile("`([^`\n]+)`").FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	return out
}

// isDiagram reports whether a fenced block is a Mermaid diagram, whose labels
// mention commands as prose rather than invoking them.
func isDiagram(block string) bool {
	for _, line := range strings.Split(block, "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 0 {
			continue
		}
		for _, k := range diagramTypes {
			if f[0] == k {
				return true
			}
		}
		return false
	}
	return false
}

// rePMMention finds a line that talks about the pmcluster CLI. A hyphen or
// underscore right after the word marks a unit, file or container name such as
// `pmcluster-private-ports.service`; flags on such a line belong to another tool
// (systemctl here), so they are not read as pmcluster flags.
var (
	rePMInvocation = regexp.MustCompile(`\bpmcluster\s+([a-z][a-z0-9-]*)`)
	rePMMention    = regexp.MustCompile(`\bpmcluster([^-_]|$)`)
	reLongFlag     = regexp.MustCompile(`(--[a-z][a-z0-9-]{1,})`)
)

func TestDocumentedCLICommandsAndFlagsExist(t *testing.T) {
	root := repoRoot(t)
	surface := codeCLISurface(t, root)
	// Global flags every cobra binary carries.
	globalFlags := map[string]bool{"help": true, "version": true, "verbose": true, "quiet": true}
	// `pmcluster daemon` is a noun in these docs — the host process, not an
	// invocation — so it is exempt. Anything else after `pmcluster` must be a
	// command this binary defines.
	proseUses := map[string]bool{"daemon": true}

	checkedCmds, checkedFlags := 0, 0
	for _, doc := range markdownDocs(t, root) {
		src := read(t, doc)
		for _, block := range fencedBlocks(src) {
			if isDiagram(block) {
				continue // a Mermaid label is not a shell command
			}
			for _, line := range strings.Split(block, "\n") {
				if !rePMMention.MatchString(line) {
					continue
				}
				if m := rePMInvocation.FindStringSubmatch(line); m != nil {
					if proseUses[m[1]] {
						continue
					}
					checkedCmds++
					if !surface.commands[m[1]] {
						t.Errorf("%s: `pmcluster %s` is not a command this binary defines",
							rel(root, doc), m[1])
					}
				}
				for _, m := range reLongFlag.FindAllStringSubmatch(line, -1) {
					name := strings.TrimPrefix(m[1], "--")
					checkedFlags++
					if surface.flags[name] || globalFlags[name] {
						continue
					}
					t.Errorf("%s: flag %s has no definition in the CLI (line: %s)",
						rel(root, doc), m[1], strings.TrimSpace(line))
				}
			}
		}
	}
	t.Logf("checked %d command invocations and %d flags against %d commands / %d flags in code",
		checkedCmds, checkedFlags, len(surface.commands), len(surface.flags))
}

// ---------------------------------------------------------------------------
// 3. every path the OpenAPI spec documents must exist as a route handler
// ---------------------------------------------------------------------------

// routeLiterals returns the string literals passed to router-registration
// calls, parsed from the AST rather than by regex. Regex on source text misses
// collection endpoints registered as `Get("/")` inside a `Route("/x", …)`
// block and reports them as non-existent.
func routeLiterals(t *testing.T, root string) map[string]string {
	t.Helper()
	routeMethods := map[string]bool{"Get": true, "Post": true, "Put": true, "Delete": true, "Patch": true}
	found := map[string]string{}
	fset := token.NewFileSet()

	// firstString returns the path a registration call is keyed on. A bare
	// literal is taken as written; a concatenation such as `prefix+"/status"`
	// yields its rightmost literal, since the left operand is a runtime prefix
	// this pass cannot resolve. The suffix match in the caller still lines the
	// route up with the spec.
	firstString := func(call *ast.CallExpr) (string, bool) {
		for _, arg := range call.Args {
			switch x := arg.(type) {
			case *ast.BasicLit:
				if x.Kind == token.STRING {
					return strings.Trim(x.Value, `"`), true
				}
			case *ast.BinaryExpr:
				if lit, ok := x.Y.(*ast.BasicLit); ok && x.Op == token.ADD && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						return s, true
					}
				}
			}
		}
		return "", false
	}

	for _, f := range goFiles(t, filepath.Join(root, "pmcluster")) {
		astFile, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		var scan func(n ast.Node, prefix string)
		scan = func(n ast.Node, prefix string) {
			ast.Inspect(n, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch {
				case sel.Sel.Name == "Route":
					// A sub-router: its prefix composes into every route inside,
					// which is how collection endpoints register (`Route("/x")`
					// + `Get("/")` is the route `/x`).
					sub, ok := firstString(call)
					if ok && strings.HasPrefix(sub, "/") {
						for _, arg := range call.Args {
							if fn, ok := arg.(*ast.FuncLit); ok {
								scan(fn.Body, prefix+sub)
							}
						}
					}
					return false // handled above; do not double-visit children
				case routeMethods[sel.Sel.Name]:
					sub, ok := firstString(call)
					if ok && strings.HasPrefix(sub, "/") {
						found[prefix+sub] = f
					}
				}
				return true
			})
		}
		scan(astFile, "")
	}
	return found
}

func normalizePath(p string) string {
	p = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(strings.TrimRight(p, "/"), "{}")
	return strings.ReplaceAll(p, "*", "{}")
}

func specPaths(t *testing.T, root string) []string {
	t.Helper()
	src := read(t, filepath.Join(root, "pmcluster", "docs", "openapi.yaml"))
	var out []string
	for _, m := range regexp.MustCompile(`(?m)^\s{2}(/[A-Za-z0-9/_{}\-]+):\s*$`).FindAllStringSubmatch(src, -1) {
		out = append(out, normalizePath(m[1]))
	}
	if len(out) == 0 {
		t.Fatal("parsed zero paths out of pmcluster/docs/openapi.yaml — the spec format changed, not the routes")
	}
	sort.Strings(out)
	return out
}

// undocumentedRoutes lists routes this spec has never covered. It may only
// shrink: documenting or deleting an entry is a deliberate act, and adding a
// new route to this list instead of to the spec is the drift this test exists
// to stop.
var undocumentedRoutes = map[string]bool{
	"/configs/{}/stack": true,
	"/health":           true,
	"/nodes/{}/storage": true,
	"/secrets/{}/stack": true,
	"/stacks/{}/ack":    true,
	"/stacks/{}/move":   true,
}

// knownUndocumented reports whether a route is on the checked-in list of
// endpoints the spec has never covered, matching on the path tail because a
// composed route may or may not carry the /api mount prefix.
func knownUndocumented(route string) bool {
	for k := range undocumentedRoutes {
		if route == k || strings.HasSuffix(route, k) || strings.HasSuffix(k, route) {
			return true
		}
	}
	return false
}

func TestOpenAPIPathsExistAndAreCovered(t *testing.T) {
	root := repoRoot(t)
	literals := routeLiterals(t, root)
	if len(literals) == 0 {
		t.Fatal("parsed zero route literals — the AST extraction is broken, not the spec")
	}
	var normalized []string
	for lit := range literals {
		normalized = append(normalized, normalizePath(lit))
	}
	sort.Strings(normalized)

	paths := specPaths(t, root)
	for _, p := range paths {
		// The router mounts most sub-packages under /api, so a registered
		// literal is normally the tail of the documented path.
		matched := false
		for _, lit := range normalized {
			if p == lit || strings.HasSuffix(p, lit) || strings.HasSuffix(lit, p) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("openapi.yaml documents %s, but no route literal in the code matches it", p)
		}
	}

	// Reverse direction: routes the spec never mentions. Reported, plus a
	// checked-in list so new gaps have to be added deliberately.
	for _, lit := range normalized {
		covered := false
		for _, p := range paths {
			if lit == p || strings.HasSuffix(p, lit) || strings.HasSuffix(lit, p) {
				covered = true
				break
			}
		}
		if !covered && !knownUndocumented(lit) {
			t.Errorf("route %s is served but absent from openapi.yaml — document it, or add it to undocumentedRoutes with a reason", lit)
		}
	}
	t.Logf("checked %d documented paths against %d route literals (%d known-undocumented routes allowed)",
		len(paths), len(normalized), len(undocumentedRoutes))
}

// ---------------------------------------------------------------------------
// 4. links and file references must resolve
// ---------------------------------------------------------------------------

var (
	reMDLink     = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	reDocRef     = regexp.MustCompile("\x60((?:docs|pmcluster|deploy)/[A-Za-z0-9_./-]+\\.(?:md|ya?ml|sh|sql))\\x60")
	reFenceStart = regexp.MustCompile("^\\s*```")
)

func TestLocalMarkdownLinksResolve(t *testing.T) {
	root := repoRoot(t)
	links := 0
	for _, doc := range markdownDocs(t, root) {
		src := read(t, doc)
		for _, m := range reMDLink.FindAllStringSubmatch(src, -1) {
			target := m[1]
			if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
				strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
				continue
			}
			target = strings.SplitN(target, "#", 2)[0]
			if target == "" {
				continue
			}
			links++
			resolved := filepath.Join(filepath.Dir(doc), target)
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s links to %s, which does not exist", rel(root, doc), target)
			}
		}
	}
	t.Logf("checked %d local links", links)
}

func TestNoReferencesToMissingFiles(t *testing.T) {
	root := repoRoot(t)
	refs := 0
	for _, doc := range markdownDocs(t, root) {
		for _, m := range reDocRef.FindAllStringSubmatch(read(t, doc), -1) {
			refs++
			// Docs trees are written both from the repository root and from
			// pmcluster/, so a reference resolves if it exists under either.
			candidates := []string{
				filepath.Join(root, m[1]),
				filepath.Join(root, "pmcluster", m[1]),
				filepath.Join(filepath.Dir(doc), m[1]),
			}
			ok := false
			for _, c := range candidates {
				if _, err := os.Stat(c); err == nil {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("%s refers to `%s`, which does not exist — a pointer to a deleted file",
					rel(root, doc), m[1])
			}
		}
	}
	t.Logf("checked %d in-repo file references", refs)
}

// ---------------------------------------------------------------------------
// 5. Mermaid blocks must parse on the first attempt
// ---------------------------------------------------------------------------

// diagramKeywords collide with Mermaid's own grammar when used as a node or
// participant id. Mermaid's keywords are case-insensitive, so `participant ALT`
// and `participant LOOP` both fail to parse while looking perfectly innocent.
var diagramKeywords = map[string]bool{
	"alt": true, "else": true, "end": true, "loop": true, "opt": true, "par": true,
	"par_over": true, "critical": true, "break": true, "rect": true, "note": true,
	"participant": true, "participant_actor": true, "actor": true, "activate": true,
	"deactivate": true, "autonumber": true, "box": true, "create": true,
	"destroy": true, "link": true, "links": true, "title": true, "over": true,
	"subgraph": true, "graph": true, "class": true, "click": true, "style": true,
	"direction": true, "left": true, "right": true,
}

var diagramTypes = []string{
	"flowchart", "graph", "sequenceDiagram", "stateDiagram-v2", "stateDiagram",
	"erDiagram", "classDiagram", "journey", "gantt", "pie", "mindmap", "timeline",
	"quadrantChart", "gitGraph", "C4Context", "block-beta",
}

var (
	reSeqParticipant = regexp.MustCompile(`(?m)^\s*participant\s+([A-Za-z0-9_]+)`)
	reNodeID         = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_]+)\s*[\[{(]`)
	reLongLRChain    = regexp.MustCompile(`-->`)
)

func mermaidBlocks(src string) []string {
	var out []string
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "```mermaid" {
			continue
		}
		var buf []string
		for j := i + 1; j < len(lines) && strings.TrimSpace(lines[j]) != "```"; j++ {
			buf = append(buf, lines[j])
		}
		out = append(out, strings.Join(buf, "\n"))
	}
	return out
}

func TestMermaidBlocksAreWellFormed(t *testing.T) {
	root := repoRoot(t)
	blocks := 0
	for _, doc := range markdownDocs(t, root) {
		for _, b := range mermaidBlocks(read(t, doc)) {
			blocks++
			body := strings.TrimSpace(b)
			if body == "" {
				t.Errorf("%s has an empty mermaid block", rel(root, doc))
				continue
			}
			first := strings.Fields(strings.SplitN(body, "\n", 2)[0])
			kind := ""
			if len(first) > 0 {
				kind = first[0]
			}
			known := false
			for _, k := range diagramTypes {
				if kind == k {
					known = true
					break
				}
			}
			if !known {
				t.Errorf("%s: mermaid block starts with %q, which is not a diagram type Mermaid recognises",
					rel(root, doc), kind)
				continue
			}

			// A `;` terminates a statement in a sequence diagram, so an HTML
			// entity in message text (&lt;volume_root&gt;) silently breaks the
			// whole block. The same entity is fine in a flowchart, which is why
			// this passes review and fails in the browser.
			if kind == "sequenceDiagram" {
				for _, line := range strings.Split(body, "\n") {
					if strings.Contains(line, "&lt;") || strings.Contains(line, "&gt;") {
						t.Errorf("%s: sequence diagram contains an HTML entity (%s) — the `;` terminates the statement; write the words instead",
							rel(root, doc), strings.TrimSpace(line))
					}
				}
			}

			// Keyword collisions: `participant LOOP as reconcile loop` does not parse.
			for _, m := range reSeqParticipant.FindAllStringSubmatch(body, -1) {
				if diagramKeywords[strings.ToLower(m[1])] {
					t.Errorf("%s: participant id %q collides with a Mermaid keyword and will not parse; rename it",
						rel(root, doc), m[1])
				}
			}

			// A single long left-to-right chain renders as a wide strip that
			// GitHub scales down until the labels are unreadable. Six links on
			// one line was ~20:1 in practice; use `flowchart TD`.
			if kind == "flowchart" && strings.Contains(first[0], "flowchart") &&
				len(first) > 1 && first[1] == "LR" {
				for _, line := range strings.Split(body, "\n") {
					if len(reLongLRChain.FindAllString(line, -1)) >= 6 {
						t.Errorf("%s: an LR chain of 6+ nodes on one line renders as an unreadable strip at GitHub's column width; use `flowchart TD`",
							rel(root, doc))
					}
				}
			}
		}
	}
	if blocks == 0 {
		t.Fatal("found no mermaid blocks at all — either the docs moved or the extractor broke")
	}
	t.Logf("checked %d mermaid blocks", blocks)
}
