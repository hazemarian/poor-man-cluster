package docscheck

// Checks pmcluster/docs/openapi.yaml against the daemon that serves it.
//
// The existing path check (TestOpenAPIPathsExistAndAreCovered) only compares
// path strings with suffix matching. It cannot see a wrong status code, a
// response field the handler does not emit, an unresolvable `$ref`, or a route
// registered with r.Method(...). Each of those drifted in this repo, so this
// file checks four things instead:
//
//  1. Every `$ref` in the spec resolves. A dangling ref is not a style problem:
//     no client generator can consume the document.
//  2. Every route the router serves is documented, and every documented
//     operation is served — both directions, including routes registered with
//     r.Method(...) and routes mounted through a sub-package's Mount(r).
//  3. The 2xx status codes the handler writes equal the 2xx codes the spec
//     documents. This is what caught `POST /api/stacks` answering 202 while the
//     spec promised 200.
//  4. The JSON keys the handler actually writes equal the documented schema's
//     properties, at the top level and one level into list/object properties.
//
// Anything the AST cannot resolve is reported with t.Logf rather than silently
// passing, so a growing "unchecked" count is visible in the test output.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// ---------------------------------------------------------------------------
// spec loading and $ref resolution
// ---------------------------------------------------------------------------

func loadOpenAPI(t *testing.T, root string) map[string]any {
	t.Helper()
	path := filepath.Join(root, "pmcluster", "docs", "openapi.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(doc) == 0 {
		t.Fatalf("%s parsed empty — the spec moved, not the code", path)
	}
	return doc
}

// resolveRef walks a #/a/b/c JSON pointer. ok is false when any segment is
// missing, which is exactly the failure mode a dangling $ref produces.
func resolveRef(doc map[string]any, ref string) (any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, true // external refs are not our business here
	}
	var cur any = doc
	for _, seg := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// collectRefs returns every $ref string in the document.
func collectRefs(node any, out *[]string) {
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			if k == "$ref" {
				if s, ok := child.(string); ok {
					*out = append(*out, s)
				}
				continue
			}
			collectRefs(child, out)
		}
	case []any:
		for _, child := range v {
			collectRefs(child, out)
		}
	}
}

// ---------------------------------------------------------------------------
// Go source index
// ---------------------------------------------------------------------------

type goPkg struct {
	dir     string
	files   map[string]*ast.File
	imports map[string]map[string]string // file → local name → import path
	funcs   map[string]*ast.FuncDecl     // package-level functions by name
	methods map[string]*ast.FuncDecl     // "Type.Name" → method
	byName  map[string][]*ast.FuncDecl   // base name → methods (ambiguity reported)
	structs map[string]*ast.StructType
}

// method returns the func for a method on typeName, falling back to a unique
// method of that name when the receiver type could not be resolved.
func (p *goPkg) method(typeName, name string) (*ast.FuncDecl, bool) {
	if typeName != "" {
		if d, ok := p.methods[typeName+"."+name]; ok {
			return d, true
		}
	}
	cands := p.byName[name]
	if len(cands) == 1 {
		return cands[0], true
	}
	// Several methods share the name (stacks.HTTP.deploy vs stacks.Service.deploy).
	// A chi handler has exactly (http.ResponseWriter, *http.Request).
	var handlers []*ast.FuncDecl
	for _, c := range cands {
		if isHTTPHandler(c) {
			handlers = append(handlers, c)
		}
	}
	if len(handlers) == 1 {
		return handlers[0], true
	}
	return nil, false
}

// isHTTPHandler reports whether fn's signature is a chi handler.
func isHTTPHandler(fn *ast.FuncDecl) bool {
	if fn.Type == nil || fn.Type.Params == nil || len(fn.Type.Params.List) != 2 {
		return false
	}
	first := fn.Type.Params.List[0].Type
	sel, ok := first.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "ResponseWriter"
}

type goIndex struct {
	fset     *token.FileSet
	pkgs     map[string]*goPkg
	dirByImp map[string]string
	root     string
}

func newGoIndex(t *testing.T, root string) *goIndex {
	t.Helper()
	idx := &goIndex{
		fset:     token.NewFileSet(),
		pkgs:     map[string]*goPkg{},
		dirByImp: map[string]string{},
		root:     root,
	}
	base := filepath.Join(root, "pmcluster")
	for _, f := range goFiles(t, base) {
		af, err := parser.ParseFile(idx.fset, f, nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		dir := filepath.Dir(f)
		pkg, ok := idx.pkgs[dir]
		if !ok {
			pkg = &goPkg{
				dir:     dir,
				files:   map[string]*ast.File{},
				imports: map[string]map[string]string{},
				funcs:   map[string]*ast.FuncDecl{},
				methods: map[string]*ast.FuncDecl{},
				byName:  map[string][]*ast.FuncDecl{},
				structs: map[string]*ast.StructType{},
			}
			idx.pkgs[dir] = pkg
		}
		pkg.files[f] = af
		pkg.imports[f] = importNames(af)
		for _, imp := range af.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if !strings.Contains(p, "/") {
				continue
			}
			// module-relative dir for our own packages
			if i := strings.Index(p, "poor-man-cluster/pmcluster/"); i >= 0 {
				idx.dirByImp[p] = filepath.Join(base, p[i+len("poor-man-cluster/pmcluster/"):])
			}
		}
		for _, decl := range af.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if rt := recvTypeName(d); rt != "" {
					pkg.methods[rt+"."+d.Name.Name] = d
					pkg.byName[d.Name.Name] = append(pkg.byName[d.Name.Name], d)
				} else {
					pkg.funcs[d.Name.Name] = d
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					if st, ok := ts.Type.(*ast.StructType); ok {
						pkg.structs[ts.Name.Name] = st
					}
				}
			}
		}
	}
	if len(idx.pkgs) == 0 {
		t.Fatal("indexed zero packages — the source walk is broken")
	}
	return idx
}

// recvTypeName returns the receiver type name of a method ("HTTP" for
// `func (h *HTTP) Mount(...)`), or "" for a package-level function.
func recvTypeName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) != 1 {
		return ""
	}
	t := d.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	switch v := t.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.IndexExpr: // generic receiver
		if id, ok := v.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

func importNames(af *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range af.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			name = p[i+1:]
		}
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = p
	}
	return out
}

// relFile renders a path for failure messages.
func (idx *goIndex) relFile(f *ast.FuncDecl) string {
	if f == nil {
		return "?"
	}
	p := idx.fset.Position(f.Pos()).Filename
	if r, err := filepath.Rel(idx.root, p); err == nil {
		return r
	}
	return p
}

// importDir resolves a package alias used in file to its directory.
func (idx *goIndex) importDir(pkg *goPkg, file *ast.File, alias string) (string, bool) {
	p, ok := pkg.imports[idx.fset.Position(file.Pos()).Filename][alias]
	if !ok {
		return "", false
	}
	d, ok := idx.dirByImp[p]
	return d, ok
}

// ---------------------------------------------------------------------------
// route extraction
// ---------------------------------------------------------------------------

type servedRoute struct {
	Method   string
	Pattern  string
	Handler  *ast.FuncDecl
	Pkg      *goPkg
	Declared string // "file:line" for messages
}

// routesServedOutsideChi are mounted by path-prefix dispatch on a raw
// http.ResponseWriter, because the chi middleware chain cannot hijack a
// connection. There is no chi route literal to read, so they are declared here
// and asserted against the spec like any other route.
var routesServedOutsideChi = []struct{ method, pattern string }{
	{"GET", "/api/services/{stack}/{service}/exec/ws"},
}

// servedRoutes reconstructs the daemon's route table by walking the chi router
// as the code builds it: start at internal/server.New, compose r.Route(prefix)
// and Group(...) nesting, and descend into every Mount(r) — including the ones
// on sub-package handlers — so a route registered in, say,
// internal/observability/http.go gets the /api prefix it actually serves under.
func (idx *goIndex) servedRoutes(t *testing.T) []servedRoute {
	t.Helper()
	var out []servedRoute
	dir := filepath.Join(idx.root, "pmcluster", "internal", "server")
	pkg := idx.pkgs[dir]
	if pkg == nil {
		t.Fatalf("no package at %s — the daemon router moved", dir)
	}
	entry := pkg.funcs["New"]
	if entry == nil || entry.Body == nil {
		t.Fatal("internal/server.New not found — the router entry point moved, and this check needs updating")
	}
	idx.walkRoutes(t, pkg, pkg.firstFile(), "", entry.Body, "", &out)
	for _, extra := range routesServedOutsideChi {
		out = append(out, servedRoute{Method: extra.method, Pattern: extra.pattern, Declared: "declared in this test"})
	}
	if len(out) == 0 {
		t.Fatal("found zero routes — the route walk is broken")
	}
	return out
}

// walkRoutes descends a function body, composing route prefixes through
// r.Route(...), r.Group(...) and sub-package Mount(r) calls.
func (idx *goIndex) walkRoutes(t *testing.T, pkg *goPkg, file *ast.File, filePath string, node ast.Node, prefix string, out *[]servedRoute) {
	t.Helper()
	descend := func(sub *goPkg, subFile *ast.File, body *ast.BlockStmt, p string) {
		idx.walkRoutes(t, sub, subFile, idx.relPathOf(subFile), body, p, out)
	}
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// Plain package-level helper that registers routes on the router it was
		// handed (BadgeMount(r, store) is the one that does this today).
		if id, ok := call.Fun.(*ast.Ident); ok {
			if helper, ok := pkg.funcs[id.Name]; ok && helper.Body != nil && helper.Body != node {
				descend(pkg, file, helper.Body, prefix)
			}
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch {
		case sel.Sel.Name == "Route" || sel.Sel.Name == "Group":
			sub := ""
			if sel.Sel.Name == "Route" {
				arg, ok := firstStringLit(call)
				if !ok {
					return true
				}
				sub = arg
			}
			for _, a := range call.Args {
				lit, ok := a.(*ast.FuncLit)
				if !ok {
					continue
				}
				// Group/Route closures keep the enclosing prefix; the Route
				// argument is its own sub-path.
				descend(pkg, file, lit.Body, prefix+sub)
			}
			return false
		case strings.HasPrefix(sel.Sel.Name, "Mount"):
			dir, typeName, ok := idx.receiverType(pkg, sel.X)
			if !ok {
				return true
			}
			sub := idx.pkgs[dir]
			if sub == nil {
				return true
			}
			mount, ok := sub.method(typeName, sel.Sel.Name)
			if !ok || mount.Body == nil {
				return true
			}
			descend(sub, sub.firstFile(), mount.Body, prefix)
			return false
		case isVerb(sel.Sel.Name):
			lit, ok := firstStringLit(call)
			if !ok || len(call.Args) < 2 {
				return true
			}
			h, hpkg := idx.resolveHandler(pkg, file, call.Args[1])
			*out = append(*out, servedRoute{
				Method:   strings.ToUpper(sel.Sel.Name),
				Pattern:  joinPattern(prefix, lit),
				Handler:  h,
				Pkg:      hpkg,
				Declared: filePath,
			})
			return true
		case sel.Sel.Name == "Method":
			if len(call.Args) < 3 {
				return true
			}
			methods := idx.methodsFor(pkg, file, call.Args[0])
			p, ok := call.Args[1].(*ast.BasicLit)
			if !ok || p.Kind != token.STRING {
				return true
			}
			pattern, _ := strconv.Unquote(p.Value)
			h, hpkg := idx.resolveHandler(pkg, file, call.Args[2])
			for _, m := range methods {
				*out = append(*out, servedRoute{
					Method:   m,
					Pattern:  joinPattern(prefix, pattern),
					Handler:  h,
					Pkg:      hpkg,
					Declared: filePath,
				})
			}
			return true
		}
		return true
	})
}

// relPathOf renders a source file relative to the repo root for messages.
func (idx *goIndex) relPathOf(f *ast.File) string {
	if f == nil {
		return "?"
	}
	p := idx.fset.Position(f.Pos()).Filename
	if r, err := filepath.Rel(idx.root, p); err == nil {
		return r
	}
	return p
}

// isVerb reports whether a chi method name registers a route.
func isVerb(name string) bool {
	switch name {
	case "Get", "Post", "Put", "Patch", "Delete":
		return true
	}
	return false
}

func joinPattern(prefix, sub string) string {
	p := prefix + sub
	if p == "" {
		return "/"
	}
	return p
}

func firstStringLit(call *ast.CallExpr) (string, bool) {
	for _, a := range call.Args {
		if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			s, err := strconv.Unquote(lit.Value)
			return s, err == nil
		}
	}
	return "", false
}

// methodsFor resolves the HTTP method argument of r.Method(...). It handles a
// literal, an http.MethodX constant, and the common
// `for _, method := range []string{http.MethodGet, http.MethodHead}` form — that
// last one is how the public badge routes are registered, and a check that
// silently skipped it would let HEAD drift.
func (idx *goIndex) methodsFor(pkg *goPkg, file *ast.File, expr ast.Expr) []string {
	if s, ok := methodName(expr); ok {
		return []string{s}
	}
	id, ok := expr.(*ast.Ident)
	if !ok {
		return nil
	}
	var methods []string
	for _, fn := range pkg.funcs {
		if fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			rng, ok := n.(*ast.RangeStmt)
			if !ok {
				return true
			}
			key, _ := rng.Value.(*ast.Ident)
			if key == nil || key.Name != id.Name {
				return true
			}
			lit, ok := rng.X.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, el := range lit.Elts {
				if s, ok := methodName(el); ok {
					methods = append(methods, s)
				}
			}
			return false
		})
	}
	return methods
}

func methodName(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(v.Value)
		return strings.ToUpper(s), err == nil
	case *ast.Ident:
		if strings.HasPrefix(v.Name, "Method") {
			return strings.ToUpper(strings.TrimPrefix(v.Name, "Method")), true
		}
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok && x.Name == "http" && strings.HasPrefix(v.Sel.Name, "Method") {
			return strings.ToUpper(strings.TrimPrefix(v.Sel.Name, "Method")), true
		}
	}
	return "", false
}

// receiverType resolves the receiver of a Mount* call to the package directory
// and type name that declare the method. It handles the three shapes the
// daemon router uses:
//
//	(&pkg.HTTP{...}).Mount(r)   — composite literal in the call
//	bh := &pkg.HTTP{...}; bh.Mount(r) — local variable
//	d.HostCerts.MountHosts(r)   — a Deps field, resolved through the field's type
func (idx *goIndex) receiverType(pkg *goPkg, expr ast.Expr) (string, string, bool) {
	if p, ok := expr.(*ast.ParenExpr); ok {
		return idx.receiverType(pkg, p.X)
	}
	if u, ok := expr.(*ast.UnaryExpr); ok {
		return idx.receiverType(pkg, u.X)
	}
	if call, ok := expr.(*ast.CallExpr); ok {
		return idx.receiverType(pkg, call.Fun)
	}
	if lit, ok := expr.(*ast.CompositeLit); ok {
		return idx.typeRefDir(pkg, lit.Type)
	}
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		// d.Field → look the field up in the package's own structs.
		return idx.fieldTypeDir(pkg, sel.Sel.Name)
	}
	id, ok := expr.(*ast.Ident)
	if !ok {
		return "", "", false
	}
	// Local variable holding a sub-package handler: find `id := &pkg.HTTP{...}`
	// anywhere in this package and resolve the composite literal's type.
	for _, fn := range pkg.funcs {
		if fn.Body == nil {
			continue
		}
		var dir, typeName string
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			lhs, ok := as.Lhs[0].(*ast.Ident)
			if !ok || lhs.Name != id.Name {
				return true
			}
			rhs := as.Rhs[0]
			if call, ok := rhs.(*ast.CallExpr); ok {
				rhs = call.Fun
			}
			if u, ok := rhs.(*ast.UnaryExpr); ok {
				rhs = u.X
			}
			lit, ok := rhs.(*ast.CompositeLit)
			if !ok {
				return true
			}
			dir, typeName, found = idx.typeRefDir(pkg, lit.Type)
			return !found
		})
		if found {
			return dir, typeName, true
		}
	}
	return "", "", false
}

// typeRefDir resolves a type expression written in a file of pkg.
func (idx *goIndex) typeRefDir(pkg *goPkg, t ast.Expr) (string, string, bool) {
	switch v := t.(type) {
	case *ast.StarExpr:
		return idx.typeRefDir(pkg, v.X)
	case *ast.Ident:
		return pkg.dir, v.Name, true
	case *ast.SelectorExpr:
		x, ok := v.X.(*ast.Ident)
		if !ok {
			return "", "", false
		}
		dir, ok := idx.aliasDir(pkg, x.Name)
		if !ok {
			return "", "", false
		}
		return dir, v.Sel.Name, true
	}
	return "", "", false
}

// fieldTypeDir resolves `d.SomeField` by finding the single struct field of that
// name in the package and following its type. Used for Deps fields such as
// `HostCerts *certs.HTTP`.
func (idx *goIndex) fieldTypeDir(pkg *goPkg, field string) (string, string, bool) {
	var dir, typeName string
	hits := 0
	for _, st := range pkg.structs {
		for _, f := range st.Fields.List {
			for _, name := range f.Names {
				if name.Name != field {
					continue
				}
				d, tn, ok := idx.typeRefDir(pkg, f.Type)
				if !ok {
					continue
				}
				dir, typeName = d, tn
				hits++
			}
		}
	}
	if hits != 1 {
		return "", "", false
	}
	return dir, typeName, true
}

// aliasDir resolves a package alias to a directory using every import block in
// the package (a handler file and its Mount file differ).
func (idx *goIndex) aliasDir(pkg *goPkg, alias string) (string, bool) {
	for path, names := range pkg.imports {
		p, ok := names[alias]
		if !ok {
			continue
		}
		if d, ok := idx.dirByImp[p]; ok {
			_ = path
			return d, true
		}
	}
	return "", false
}

func (p *goPkg) firstFile() *ast.File {
	if len(p.files) == 0 {
		return nil
	}
	paths := make([]string, 0, len(p.files))
	for path := range p.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return p.files[paths[0]]
}

func (p *goPkg) firstFileName() string {
	if len(p.files) == 0 {
		return ""
	}
	paths := make([]string, 0, len(p.files))
	for path := range p.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths[0]
}

// resolveHandler maps a chi handler expression to its declaration.
// Handles: api.Health, http.HandlerFunc(f), h.list, and api.NodesHandler(dep…).
func (idx *goIndex) resolveHandler(pkg *goPkg, file *ast.File, expr ast.Expr) (*ast.FuncDecl, *goPkg) {
	switch v := expr.(type) {
	case *ast.CallExpr:
		if d, p := idx.resolveHandler(pkg, file, v.Fun); d != nil {
			return d, p
		}
		for _, arg := range v.Args {
			if d, p := idx.resolveHandler(pkg, file, arg); d != nil {
				return d, p
			}
		}
		return nil, nil
	case *ast.UnaryExpr:
		return idx.resolveHandler(pkg, file, v.X)
	case *ast.ParenExpr:
		return idx.resolveHandler(pkg, file, v.X)
	case *ast.Ident:
		if d, ok := pkg.funcs[v.Name]; ok {
			return d, pkg
		}
		if d, ok := pkg.method("", v.Name); ok {
			return d, pkg
		}
		// A handler built from a closure: `h := func(w, r) {...}`.
		for _, cand := range pkg.funcs {
			if cand.Body == nil {
				continue
			}
			var lit *ast.FuncLit
			ast.Inspect(cand.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
					return true
				}
				lhs, ok := as.Lhs[0].(*ast.Ident)
				if !ok || lhs.Name != v.Name {
					return true
				}
				if fl, ok := as.Rhs[0].(*ast.FuncLit); ok {
					lit = fl
					return false
				}
				return true
			})
			if lit != nil {
				return &ast.FuncDecl{Name: ast.NewIdent(v.Name), Type: lit.Type, Body: lit.Body}, pkg
			}
		}
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			if dir, ok := idx.aliasDir(pkg, x.Name); ok {
				if sub := idx.pkgs[dir]; sub != nil {
					if d, ok := sub.funcs[v.Sel.Name]; ok {
						return d, sub
					}
					if d, ok := sub.method("", v.Sel.Name); ok {
						return d, sub
					}
				}
				return nil, nil
			}
			// Method value on a receiver (h.list): same package.
			if d, ok := pkg.funcs[v.Sel.Name]; ok {
				return d, pkg
			}
			if d, ok := pkg.method("", v.Sel.Name); ok {
				return d, pkg
			}
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// response contract: 2xx status codes + JSON shape
// ---------------------------------------------------------------------------

type shape struct {
	Keys     map[string]bool
	Children map[string]*shape
	// Type is the Go composite-literal type the keys came from, when known.
	// It is used to resolve later field assignments (dto.Error = …) back to
	// their JSON names; the spec side leaves it nil.
	Type ast.Expr
}

func newShape() *shape { return &shape{Keys: map[string]bool{}, Children: map[string]*shape{}} }

type contract struct {
	Statuses map[int]bool
	Body     *shape // union of every 2xx JSON body the handler can write
}

// mergeShape unions src into dst. A handler with a degraded branch ("source
// unavailable") writes a partial object; the documented schema is the union, so
// a spec cannot pass by describing only the happy or only the degraded path.
func mergeShape(dst *shape, src *shape) {
	if src == nil {
		return
	}
	if dst.Type == nil {
		dst.Type = src.Type
	}
	for k := range src.Keys {
		dst.Keys[k] = true
	}
	for k, child := range src.Children {
		if existing, ok := dst.Children[k]; ok {
			mergeShape(existing, child)
			continue
		}
		dst.Children[k] = child
	}
}

var httpStatus = map[string]int{
	"StatusOK":                    200,
	"StatusCreated":               201,
	"StatusAccepted":              202,
	"StatusNoContent":             204,
	"StatusBadRequest":            400,
	"StatusUnauthorized":          401,
	"StatusForbidden":             403,
	"StatusNotFound":              404,
	"StatusConflict":              409,
	"StatusInternalServerError":   500,
	"StatusServiceUnavailable":    503,
	"StatusBadGateway":            502,
	"StatusNotImplemented":        501,
	"StatusMethodNotAllowed":      405,
	"StatusUnprocessableEntity":   422,
	"StatusTooManyRequests":       429,
	"StatusGatewayTimeout":        504,
	"StatusRequestEntityTooLarge": 413,
}

func statusValue(e ast.Expr) (int, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.INT {
			n, err := strconv.Atoi(v.Value)
			return n, err == nil
		}
	case *ast.SelectorExpr:
		if n, ok := httpStatus[v.Sel.Name]; ok {
			return n, true
		}
	}
	return 0, false
}

func (idx *goIndex) contract(pkg *goPkg, fn *ast.FuncDecl) contract {
	c := contract{Statuses: map[int]bool{}, Body: newShape()}
	idx.scanWrites(pkg, fn, &c, 0, map[*ast.FuncDecl]bool{})
	if len(c.Body.Keys) == 0 {
		c.Body = nil
	}
	return c
}

// scanWrites collects the 2xx codes and the JSON body a handler can write. It
// follows same-package response helpers (writeEvidence, writeJSON wrappers),
// because the daemon writes several endpoints through one shared writer.
func (idx *goIndex) scanWrites(pkg *goPkg, fn *ast.FuncDecl, c *contract, depth int, seen map[*ast.FuncDecl]bool) {
	if fn == nil || fn.Body == nil || seen[fn] || depth > 3 {
		return
	}
	seen[fn] = true
	var callees []*ast.FuncDecl
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch f := call.Fun.(type) {
		case *ast.Ident:
			name = f.Name
		case *ast.SelectorExpr:
			name = f.Sel.Name
		}
		switch name {
		case "writeJSON":
			if len(call.Args) >= 3 {
				if st, ok := statusValue(call.Args[1]); ok {
					c.Statuses[st] = true
					if st >= 200 && st < 300 {
						if got := idx.shapeOf(pkg, fn, call.Args[2], 0, map[string]bool{}); got != nil {
							mergeShape(c.Body, got)
						}
					}
				}
			}
			return true
		case "WriteHeader":
			if len(call.Args) == 1 {
				if st, ok := statusValue(call.Args[0]); ok {
					c.Statuses[st] = true
				}
			}
			return true
		}
		// A same-package helper called from this handler may be the writer.
		if callee, cpkg := idx.resolveHandler(pkg, pkg.firstFile(), call.Fun); callee != nil && cpkg == pkg && callee != fn {
			callees = append(callees, callee)
		}
		return true
	})
	for _, callee := range callees {
		idx.scanWrites(pkg, callee, c, depth+1, seen)
	}
}

// shapeOf resolves a response payload expression to the set of JSON keys it
// emits. It follows same-package helpers (stackJSON, toStatusDTO,
// serviceJSONList) and local variables assigned a composite literal, which is
// how every handler in this daemon builds its body.
func (idx *goIndex) shapeOf(pkg *goPkg, fn *ast.FuncDecl, expr ast.Expr, depth int, seen map[string]bool) *shape {
	if expr == nil || depth > 4 {
		return nil
	}
	switch v := expr.(type) {
	case *ast.UnaryExpr:
		return idx.shapeOf(pkg, fn, v.X, depth+1, seen)
	case *ast.ParenExpr:
		return idx.shapeOf(pkg, fn, v.X, depth+1, seen)
	case *ast.CompositeLit:
		return idx.compositeShape(pkg, fn, v, depth, seen)
	case *ast.Ident:
		if seen["var:"+v.Name] || fn == nil {
			return nil
		}
		seen["var:"+v.Name] = true
		var out *shape
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			// Conditional keys: out["finished_at"] = … after the literal.
			if as, ok := n.(*ast.AssignStmt); ok && len(as.Lhs) == 1 && len(as.Rhs) == 1 {
				if idxExpr, ok := as.Lhs[0].(*ast.IndexExpr); ok {
					if base, ok := idxExpr.X.(*ast.Ident); ok && base.Name == v.Name {
						if key, ok := stringKey(idxExpr.Index); ok && out != nil {
							out.Keys[key] = true
						}
					}
				}
			}
			if out != nil {
				return true
			}
			switch node := n.(type) {
			case *ast.AssignStmt:
				if len(node.Rhs) != 1 && len(node.Lhs) != len(node.Rhs) {
					return true
				}
				idxMatch := -1
				for i, l := range node.Lhs {
					if lid, ok := l.(*ast.Ident); ok && lid.Name == v.Name {
						idxMatch = i
						break
					}
				}
				if idxMatch < 0 {
					return true
				}
				rhs := node.Rhs[min(idxMatch, len(node.Rhs)-1)]
				// x = append(x, lit)
				if call, ok := rhs.(*ast.CallExpr); ok && len(call.Args) == 2 {
					rhs = call.Args[1]
				}
				out = idx.shapeOf(pkg, fn, rhs, depth+1, seen)
				return false
			case *ast.ValueSpec:
				for i, name := range node.Names {
					if name.Name != v.Name || i >= len(node.Values) {
						continue
					}
					out = idx.shapeOf(pkg, fn, node.Values[i], depth+1, seen)
				}
				return false
			}
			return true
		})
		if out != nil {
			idx.addFieldAssignments(pkg, fn, v.Name, out)
		}
		return out
	case *ast.CallExpr:
		callee, cpkg := idx.resolveHandler(pkg, pkg.firstFile(), v)
		if callee == nil {
			// Interface call (h.Svc.Get(...)): the concrete implementation in
			// the same package documents the response shape.
			if sel, ok := v.Fun.(*ast.SelectorExpr); ok {
				if d, ok := pkg.method("", sel.Sel.Name); ok {
					callee, cpkg = d, pkg
				}
			}
		}
		if callee == nil || callee.Body == nil || cpkg == nil {
			return nil
		}
		key := "fn:" + cpkg.dir + "." + callee.Name.Name
		if seen[key] {
			return nil
		}
		seen[key] = true
		// Union across every return: a helper's first return is usually the
		// error path ("return nil, err"), and the body is built further down.
		out := newShape()
		ast.Inspect(callee.Body, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok || len(ret.Results) == 0 {
				return true
			}
			if got := idx.shapeOf(cpkg, callee, ret.Results[0], depth+1, seen); got != nil {
				mergeShape(out, got)
			}
			return true
		})
		if len(out.Keys) == 0 {
			return nil
		}
		return out
	}
	return nil
}

// addFieldAssignments folds `x.Field = …` assignments back into the shape of x,
// mapping the Go field name to its JSON tag. Handlers use this to set optional
// fields after building the literal (dto.Error = … on a degraded branch), and a
// schema that omitted those keys would be wrong exactly when things go wrong.
func (idx *goIndex) addFieldAssignments(pkg *goPkg, fn *ast.FuncDecl, varName string, out *shape) {
	if out == nil || out.Type == nil || fn == nil {
		return
	}
	tags := idx.structTags(pkg, out.Type)
	if len(tags) == 0 {
		return
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 {
			return true
		}
		sel, ok := as.Lhs[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		base, ok := sel.X.(*ast.Ident)
		if !ok || base.Name != varName {
			return true
		}
		if name, ok := tags[sel.Sel.Name]; ok {
			out.Keys[name] = true
		}
		return true
	})
}

func (idx *goIndex) compositeShape(pkg *goPkg, fn *ast.FuncDecl, lit *ast.CompositeLit, depth int, seen map[string]bool) *shape {
	// Slice literal of a single map/struct element: []map[string]any{{…}}.
	if _, isMap := lit.Type.(*ast.MapType); !isMap {
		if t, ok := lit.Type.(*ast.ArrayType); ok {
			if _, inner := t.Elt.(*ast.MapType); inner || isStructTypeExpr(t.Elt) {
				var out *shape
				for _, el := range lit.Elts {
					if innerLit, ok := el.(*ast.CompositeLit); ok {
						out = idx.compositeShape(pkg, fn, innerLit, depth+1, seen)
					}
				}
				return out
			}
			return nil
		}
	}
	// map[string]any{...}
	if mt, ok := lit.Type.(*ast.MapType); ok {
		if id, ok := mt.Key.(*ast.Ident); !ok || id.Name != "string" {
			return nil
		}
		out := newShape()
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := stringKey(kv.Key)
			if !ok {
				continue
			}
			out.Keys[key] = true
			if child := idx.shapeOf(pkg, fn, kv.Value, depth+1, seen); child != nil && len(child.Keys) > 0 {
				out.Children[key] = child
			}
		}
		if len(out.Keys) == 0 {
			return nil
		}
		out.Type = lit.Type
		return out
	}
	// struct literal: statusDTO{…} / &siteCertResponse{…}
	tags := idx.structTags(pkg, lit.Type)
	if len(tags) == 0 {
		return nil
	}
	out := newShape()
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			// positional literal: map by field order
			continue
		}
		id, ok := kv.Key.(*ast.Ident)
		if !ok {
			continue
		}
		name, ok := tags[id.Name]
		if !ok {
			continue
		}
		out.Keys[name] = true
		if child := idx.shapeOf(pkg, fn, kv.Value, depth+1, seen); child != nil && len(child.Keys) > 0 {
			out.Children[name] = child
		}
	}
	if len(out.Keys) == 0 {
		return nil
	}
	out.Type = lit.Type
	return out
}

func isStructTypeExpr(e ast.Expr) bool {
	switch e.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return true
	}
	return false
}

func stringKey(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// structTags maps Go field names to JSON names for a struct type expression.
func (idx *goIndex) structTags(pkg *goPkg, expr ast.Expr) map[string]string {
	out := map[string]string{}
	var st *ast.StructType
	switch v := expr.(type) {
	case *ast.StructType:
		st = v
	case *ast.Ident:
		st = pkg.structs[v.Name]
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			if dir, ok := idx.dirByImp[pkg.imports[pkg.firstFileName()][x.Name]]; ok {
				if sub := idx.pkgs[dir]; sub != nil {
					st = sub.structs[v.Sel.Name]
				}
			}
		}
	}
	if st == nil {
		return out
	}
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		name := jsonName(field)
		if name == "" {
			continue
		}
		for _, n := range field.Names {
			out[n.Name] = name
		}
	}
	return out
}

func jsonName(field *ast.Field) string {
	if field.Tag == nil {
		return ""
	}
	tag, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return ""
	}
	for _, part := range strings.Split(tag, " ") {
		if !strings.HasPrefix(part, `json:"`) {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(part, `json:"`), `"`)
		name := strings.Split(raw, ",")[0]
		if name == "-" {
			return ""
		}
		if name == "" {
			// `json:",omitempty"` → field name is used; fall back to Go name.
			return field.Names[0].Name
		}
		return name
	}
	return ""
}

// ---------------------------------------------------------------------------
// 1. every $ref resolves
// ---------------------------------------------------------------------------

func TestOpenAPIRefsResolve(t *testing.T) {
	doc := loadOpenAPI(t, repoRoot(t))
	var refs []string
	collectRefs(doc, &refs)
	if len(refs) == 0 {
		t.Fatal("spec declares zero $refs — the parser or the spec changed shape")
	}
	broken := map[string]bool{}
	for _, ref := range refs {
		if _, ok := resolveRef(doc, ref); !ok {
			broken[ref] = true
		}
	}
	for _, ref := range sortedKeys(broken) {
		t.Errorf("openapi.yaml references %s, which does not exist — a dangling $ref makes the whole document unusable for generated clients", ref)
	}

	// 3.0.3 forbids 3.1 keywords; a schema using them is rejected by validators.
	var walk func(any, string)
	walk = func(node any, path string) {
		switch v := node.(type) {
		case map[string]any:
			for k, child := range v {
				if k == "const" {
					t.Errorf("%s: `const` is an OpenAPI 3.1/JSON-Schema keyword; this document declares %v — use `enum` with one value",
						path, doc["openapi"])
				}
				walk(child, path+"/"+k)
			}
		case []any:
			for i, child := range v {
				walk(child, fmt.Sprintf("%s[%d]", path, i))
			}
		}
	}
	walk(doc, "")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 2. routes and the spec agree in both directions
// ---------------------------------------------------------------------------

func specOperations(t *testing.T, doc map[string]any) map[string]map[string]map[string]any {
	t.Helper()
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		t.Fatal("spec has no paths object")
	}
	out := map[string]map[string]map[string]any{}
	for path, ops := range paths {
		m, ok := ops.(map[string]any)
		if !ok {
			continue
		}
		norm := normalizeRoute(path)
		out[norm] = map[string]map[string]any{}
		for method, op := range m {
			up := strings.ToUpper(method)
			switch up {
			case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
				o, _ := op.(map[string]any)
				out[norm][up] = o
			}
		}
	}
	return out
}

// normalizeRoute rewrites every parameter segment to {}, so /a/{stack}/b and
// /a/{name}/b compare equal — the names are documentation, not routing.
func normalizeRoute(p string) string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, "{}")
			continue
		}
		out = append(out, seg)
	}
	return strings.Join(out, "/")
}

func TestOpenAPIDocumentsEveryServedRoute(t *testing.T) {
	root := repoRoot(t)
	doc := loadOpenAPI(t, root)
	ops := specOperations(t, doc)
	idx := newGoIndex(t, root)

	served := map[string]servedRoute{}
	for _, r := range idx.servedRoutes(t) {
		key := r.Method + " " + normalizeRoute(r.Pattern)
		if prev, dup := served[key]; dup {
			t.Logf("route %s registered twice (%s and %s)", key, prev.Declared, r.Declared)
		}
		served[key] = r
	}

	var undocumented, unserved []string
	for key, r := range served {
		path := normalizeRoute(r.Pattern)
		if _, ok := ops[path]; !ok {
			undocumented = append(undocumented, fmt.Sprintf("%s (registered at %s)", key, r.Declared))
			continue
		}
		if _, ok := ops[path][r.Method]; !ok {
			undocumented = append(undocumented, fmt.Sprintf("%s (route exists, method absent from the spec; registered at %s)", key, r.Declared))
		}
	}
	for path, methods := range ops {
		for method := range methods {
			if _, ok := served[method+" "+path]; !ok {
				unserved = append(unserved, method+" "+path)
			}
		}
	}
	sort.Strings(undocumented)
	sort.Strings(unserved)
	for _, u := range undocumented {
		t.Errorf("served but not documented: %s — document it in openapi.yaml, or the console's own API client drifts from the daemon", u)
	}
	for _, u := range unserved {
		t.Errorf("documented but not served: %s — the spec promises an endpoint the router does not register", u)
	}
	t.Logf("compared %d served routes against %d documented operations", len(served), countOps(ops))
}

func countOps(ops map[string]map[string]map[string]any) int {
	n := 0
	for _, methods := range ops {
		n += len(methods)
	}
	return n
}

// ---------------------------------------------------------------------------
// 3+4. status codes and JSON shapes
// ---------------------------------------------------------------------------

func TestOpenAPIResponseMatchesHandlers(t *testing.T) {
	root := repoRoot(t)
	doc := loadOpenAPI(t, root)
	ops := specOperations(t, doc)
	idx := newGoIndex(t, root)

	routes := idx.servedRoutes(t)
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Pattern == routes[j].Pattern {
			return routes[i].Method < routes[j].Method
		}
		return routes[i].Pattern < routes[j].Pattern
	})

	unchecked, checked := 0, 0
	for _, r := range routes {
		if r.Handler == nil {
			if specHasJSONSchema(ops[normalizeRoute(r.Pattern)][r.Method]) {
				t.Logf("no handler resolved for %s %s — response unchecked", r.Method, r.Pattern)
				unchecked++
			}
			continue
		}
		op, ok := ops[normalizeRoute(r.Pattern)][r.Method]
		if !ok {
			continue // route coverage test reports this
		}
		c := idx.contract(r.Pkg, r.Handler)
		codeCodes := map[int]bool{}
		for st := range c.Statuses {
			if st >= 200 && st < 300 {
				codeCodes[st] = true
			}
		}
		specCodes := specSuccessCodes(op)
		if len(codeCodes) > 0 && !sameIntSet(codeCodes, specCodes) {
			t.Errorf("%s %s: handler answers %s, spec documents %s (%s)",
				r.Method, r.Pattern, formatCodes(codeCodes), formatCodes(specCodes), idx.relFile(r.Handler))
			continue
		}
		if c.Body == nil || len(c.Body.Keys) == 0 {
			// No resolvable JSON body (SVG, websocket, or a body built by a
			// function this analysis cannot follow). The status set above is
			// still asserted; say out loud that the shape is not.
			if specHasJSONSchema(op) {
				t.Logf("%s %s: handler body not statically resolvable, shape unchecked (%s)", r.Method, r.Pattern, idx.relFile(r.Handler))
				unchecked++
			}
			continue
		}
		if os.Getenv("OPENAPI_SHAPES") != "" {
			t.Logf("SHAPE %s %s = %s", r.Method, r.Pattern, describeShape(c.Body))
		}
		specBody := specSuccessShape(doc, op)
		if specBody == nil {
			t.Errorf("%s %s: handler writes a JSON object with %d keys, the spec documents no response schema (%s)",
				r.Method, r.Pattern, len(c.Body.Keys), idx.relFile(r.Handler))
			continue
		}
		checked++
		compareShapes(t, r, "", c.Body, specBody, idx)
	}
	t.Logf("checked response shapes on %d routes; %d left unchecked (see per-route notes above)", checked, unchecked)
}

// describeShape renders a shape for debugging (OPENAPI_SHAPES=1).
func describeShape(s *shape) string {
	if s == nil {
		return "-"
	}
	keys := make([]string, 0, len(s.Keys))
	for k := range s.Keys {
		if _, ok := s.Children[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := "{" + strings.Join(keys, ",") + "}"
	for k, child := range s.Children {
		out += " " + k + ":" + describeShape(child)
	}
	return out
}

func compareShapes(t *testing.T, r servedRoute, path string, code *shape, spec *shape, idx *goIndex) {
	t.Helper()
	where := fmt.Sprintf("%s %s%s", r.Method, r.Pattern, path)
	var missingInSpec, missingInCode []string
	for k := range code.Keys {
		if !spec.Keys[k] {
			missingInSpec = append(missingInSpec, k)
		}
	}
	for k := range spec.Keys {
		if !code.Keys[k] {
			missingInCode = append(missingInCode, k)
		}
	}
	sort.Strings(missingInSpec)
	sort.Strings(missingInCode)
	if len(missingInSpec) > 0 {
		t.Errorf("%s: handler emits %v, which the documented schema omits (%s)", where, missingInSpec, idx.relFile(r.Handler))
	}
	if len(missingInCode) > 0 {
		t.Errorf("%s: spec documents %v, which the handler never emits (%s)", where, missingInCode, idx.relFile(r.Handler))
	}
	for k, child := range code.Children {
		sc, ok := spec.Children[k]
		if !ok || sc == nil || len(sc.Keys) == 0 {
			continue
		}
		compareShapes(t, r, path+"."+k, child, sc, idx)
	}
}

// specSuccessCodes returns the 2xx status codes the spec documents.
func specSuccessCodes(op map[string]any) map[int]bool {
	out := map[int]bool{}
	resp, _ := op["responses"].(map[string]any)
	for code := range resp {
		n, err := strconv.Atoi(code)
		if err == nil && n >= 200 && n < 300 {
			out[n] = true
		}
	}
	return out
}

func specHasJSONSchema(op map[string]any) bool {
	return specBodySchema(op) != nil
}

// specBodySchema returns the JSON schema of the first 2xx response that has one.
func specBodySchema(op map[string]any) map[string]any {
	resp, _ := op["responses"].(map[string]any)
	codes := make([]string, 0, len(resp))
	for code := range resp {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		r, _ := resp[code].(map[string]any)
		content, _ := r["content"].(map[string]any)
		j, _ := content["application/json"].(map[string]any)
		schema, _ := j["schema"].(map[string]any)
		if schema != nil {
			return schema
		}
	}
	return nil
}

func specSuccessShape(doc map[string]any, op map[string]any) *shape {
	schema := specBodySchema(op)
	if schema == nil {
		return nil
	}
	return specShapeOf(doc, schema, 0)
}

// specShapeOf builds the same key tree from a documented schema, resolving
// $ref and looking one level into array items and object properties.
func specShapeOf(doc map[string]any, schema map[string]any, depth int) *shape {
	if schema == nil || depth > 4 {
		return nil
	}
	if ref, ok := schema["$ref"].(string); ok {
		target, ok := resolveRef(doc, ref)
		if !ok {
			return nil
		}
		m, _ := target.(map[string]any)
		return specShapeOf(doc, m, depth+1)
	}
	out := newShape()
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		items, _ := schema["items"].(map[string]any)
		if items != nil {
			return specShapeOf(doc, items, depth+1)
		}
		return nil
	}
	for name, propAny := range props {
		out.Keys[name] = true
		prop, _ := propAny.(map[string]any)
		if prop == nil {
			continue
		}
		if child := specShapeOf(doc, prop, depth+1); child != nil && len(child.Keys) > 0 {
			out.Children[name] = child
		}
	}
	if len(out.Keys) == 0 {
		return nil
	}
	return out
}

func sameIntSet(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func formatCodes(m map[int]bool) string {
	codes := make([]int, 0, len(m))
	for c := range m {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	parts := make([]string, len(codes))
	for i, c := range codes {
		parts[i] = strconv.Itoa(c)
	}
	return "[" + strings.Join(parts, " ") + "]"
}
