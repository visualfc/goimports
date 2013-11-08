package main

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"code.google.com/p/go.tools/astutil"
)

func fixImports(f *ast.File) error {
	// refs are a set of possible package references currently unsatisified by imports.
	// first key: either base package (e.g. "fmt") or renamed package
	// second key: referenced package symbol (e.g. "Println")
	refs := make(map[string]map[string]bool)

	// decls are the current package imports. key is base package or renamed package.
	decls := make(map[string]*ast.ImportSpec)

	// collect potential uses of packages.
	var visitor visitFn
	visitor = visitFn(func(node ast.Node) ast.Visitor {
		if node == nil {
			return visitor
		}
		switch v := node.(type) {
		case *ast.ImportSpec:
			if v.Name != nil {
				decls[v.Name.Name] = v
			} else {
				local := path.Base(strings.Trim(v.Path.Value, `\"`))
				decls[local] = v
			}
		case *ast.SelectorExpr:
			xident, ok := v.X.(*ast.Ident)
			if !ok {
				break
			}
			if xident.Obj != nil {
				// if the parser can resolve it, it's not a package ref
				break
			}
			pkgName := xident.Name
			if refs[pkgName] == nil {
				refs[pkgName] = make(map[string]bool)
			}
			if decls[pkgName] == nil {
				refs[pkgName][v.Sel.Name] = true
			}
		}
		return visitor
	})
	ast.Walk(visitor, f)

	// Search for imports matching potential package references.
	searches := 0
	type result struct {
		ipath string
		err   error
	}
	results := make(chan result)
	for pkgName, symbols := range refs {
		if len(symbols) == 0 {
			continue // skip over packages already imported
		}
		go func(pkgName string, symbols map[string]bool) {
			ipath, err := findImport(pkgName, symbols)
			results <- result{ipath, err}
		}(pkgName, symbols)
		searches++
	}
	for i := 0; i < searches; i++ {
		result := <-results
		if result.err != nil {
			return result.err
		}
		if result.ipath != "" {
			astutil.AddImport(f, result.ipath)
		}
	}

	// Nil out any unused ImportSpecs, to be removed in following passes
	unusedImport := map[string]bool{}
	for pkg, is := range decls {
		if refs[pkg] == nil && pkg != "_" && pkg != "." {
			unusedImport[strings.Trim(is.Path.Value, `"`)] = true
		}
	}
	for ipath := range unusedImport {
		astutil.DeleteImport(f, ipath)
	}

	return nil
}

type pkg struct {
	importpath string // full pkg import path, e.g. "net/http"
	dir        string // absolute file path to pkg directory e.g. "/usr/lib/go/src/fmt"
}

var pkgIndexOnce sync.Once

var pkgIndex struct {
	sync.Mutex
	m map[string][]pkg // shortname => []pkg, e.g "http" => "net/http"
}

func loadPkgIndex() {
	pkgIndex.Lock()
	pkgIndex.m = make(map[string][]pkg)
	pkgIndex.Unlock()

	var wg sync.WaitGroup
	for _, path := range build.Default.SrcDirs() {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprint(os.Stderr, err)
			continue
		}
		children, err := f.Readdir(-1)
		f.Close()
		if err != nil {
			fmt.Fprint(os.Stderr, err)
			continue
		}
		for _, child := range children {
			if child.IsDir() {
				wg.Add(1)
				go func(path, name string) {
					defer wg.Done()
					loadPkg(&wg, path, name)
				}(path, child.Name())
			}
		}
	}
	wg.Wait()
}

var fset = token.NewFileSet()

func loadPkg(wg *sync.WaitGroup, root, importpath string) {
	shortName := path.Base(importpath)

	dir := filepath.Join(root, importpath)
	pkgIndex.Lock()
	pkgIndex.m[shortName] = append(pkgIndex.m[shortName], pkg{
		importpath: importpath,
		dir:        dir,
	})
	pkgIndex.Unlock()

	pkgDir, err := os.Open(dir)
	if err != nil {
		return
	}
	children, err := pkgDir.Readdir(-1)
	pkgDir.Close()
	if err != nil {
		return
	}
	for _, child := range children {
		if strings.HasPrefix(child.Name(), ".") {
			continue
		}
		if child.IsDir() {
			wg.Add(1)
			go func(root, name string) {
				defer wg.Done()
				loadPkg(wg, root, name)
			}(root, filepath.Join(importpath, child.Name()))
		}
	}
}

// loadExports returns a list exports for a package.
func loadExports(dir string) map[string]bool {
	exports := make(map[string]bool)
	buildPkg, err := build.ImportDir(dir, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not import %q: %v", dir, err)
		return nil
	}
	for _, file := range buildPkg.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(dir, file), nil, 0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not parse %q: %v", file, err)
			continue
		}
		for name := range f.Scope.Objects {
			if ast.IsExported(name) {
				exports[name] = true
			}
		}
	}
	return exports
}

// findImport searches for a package with the given symbols.
// If no package is found, findImport returns "".
// Declared as a variable rather than a function so goimports can be easily
// extended by adding a file with an init function.
var findImport = func(pkgName string, symbols map[string]bool) (string, error) {
	pkgIndexOnce.Do(loadPkgIndex)

	// Collect exports for packages with matching names.
	var wg sync.WaitGroup
	var pkgsMu sync.Mutex // guards pkgs
	// full importpath => exported symbol => True
	// e.g. "net/http" => "Client" => True
	pkgs := make(map[string]map[string]bool)
	pkgIndex.Lock()
	for _, pkg := range pkgIndex.m[pkgName] {
		wg.Add(1)
		go func(importpath, dir string) {
			defer wg.Done()
			exports := loadExports(dir)
			if exports != nil {
				pkgsMu.Lock()
				pkgs[importpath] = exports
				pkgsMu.Unlock()
			}
		}(pkg.importpath, pkg.dir)
	}
	pkgIndex.Unlock()
	wg.Wait()

	// Filter out packages missing required exported symbols.
	for symbol := range symbols {
		for importpath, exports := range pkgs {
			if !exports[symbol] {
				delete(pkgs, importpath)
			}
		}
	}
	if len(pkgs) == 0 {
		return "", nil
	}

	// If there are multiple candidate packages, the shortest one wins.
	// This is a heuristic to prefer the standard library (e.g. "bytes")
	// over e.g. "github.com/foo/bar/bytes".
	shortest := ""
	for importPath := range pkgs {
		if shortest == "" || len(importPath) < len(shortest) {
			shortest = importPath
		}
	}
	return shortest, nil
}

type visitFn func(node ast.Node) ast.Visitor

func (fn visitFn) Visit(node ast.Node) ast.Visitor {
	return fn(node)
}
