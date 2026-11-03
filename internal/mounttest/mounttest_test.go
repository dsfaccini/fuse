// Copyright 2026 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mounttest

import (
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestSkipUnlessEnabledSkipsByDefault(t *testing.T) {
	t.Setenv(Env, "")
	reached := false
	t.Run("gated", func(t *testing.T) {
		SkipUnlessEnabled(t)
		reached = true
	})
	if reached {
		t.Errorf("SkipUnlessEnabled did not skip with %s unset", Env)
	}
}

// TestMountingTestsAreGated runs ungatedMounts on the test files of every
// directory in the module.
func TestMountingTestsAreGated(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root: %v", err)
	}

	fset := token.NewFileSet()
	filesByDir := map[string][]*ast.File{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		dir := filepath.Dir(path)
		filesByDir[dir] = append(filesByDir[dir], file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	checked := 0
	for _, files := range filesByDir {
		errs, n := ungatedMounts(fset, files)
		checked += n
		for _, err := range errs {
			t.Error(err)
		}
	}
	if checked == 0 {
		t.Fatal("found no mounting tests; the scan is broken")
	}
}

func TestUngatedMountsFindsIndirectMounts(t *testing.T) {
	for _, test := range []struct {
		name    string
		src     string
		wantErr bool
	}{
		{
			name: "helper",
			src: `package p
func mountMinimalFS(t *testing.T) { fuse.Mount("", nil, nil) }
func TestMountViaHelper(t *testing.T) { mountMinimalFS(t) }`,
			wantErr: true,
		},
		{
			name: "gated helper",
			src: `package p
func mountMinimalFS(t *testing.T) { fuse.Mount("", nil, nil) }
func TestMountViaHelper(t *testing.T) {
	mounttest.SkipUnlessEnabled(t)
	mountMinimalFS(t)
}`,
		},
		{
			name: "example with output",
			src: `package p
func ExampleMount() {
	fuse.Mount("", nil, nil)
	// Output:
}`,
			wantErr: true,
		},
		{
			name: "example without output",
			src: `package p
func ExampleMount() { fuse.Mount("", nil, nil) }`,
		},
		{
			name: "direct SetUp",
			src: `package p
func TestHelloSetUpDirect(t *testing.T) { (&samples.SampleTest{}).SetUp(nil) }`,
			wantErr: true,
		},
		{
			name: "fuzz target",
			src: `package p
func FuzzMount(f *testing.F) { f.Fuzz(func(t *testing.T, dir string) { fuse.Mount(dir, nil, nil) }) }`,
			wantErr: true,
		},
		{
			name: "named subtest",
			src: `package p
func mountSub(t *testing.T) { fuse.Mount("", nil, nil) }
func TestNamedSubtest(t *testing.T) { t.Run("mount", mountSub) }`,
			wantErr: true,
		},
		{
			name: "named fuzz function",
			src: `package p
func fuzzMount(t *testing.T, dir string) { fuse.Mount(dir, nil, nil) }
func FuzzNamed(f *testing.F) { f.Fuzz(fuzzMount) }`,
			wantErr: true,
		},
		{
			name: "function table",
			src: `package p
func mountSub(t *testing.T) { fuse.Mount("", nil, nil) }
func TestTable(t *testing.T) {
	for _, fn := range []func(*testing.T){mountSub} {
		fn(t)
	}
}`,
			wantErr: true,
		},
		{
			name: "package-level func var",
			src: `package p
var mountVar = func(t *testing.T) { fuse.Mount("", nil, nil) }
func TestFuncVar(t *testing.T) { mountVar(t) }`,
			wantErr: true,
		},
		{
			name: "init",
			src: `package p
func init() { fuse.Mount("", nil, nil) }`,
			wantErr: true,
		},
		{
			name: "init registering a suite",
			src: `package p
func init() { RegisterTestSuite(&HelloFSTest{}) }`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "p_test.go", test.src, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			errs, _ := ungatedMounts(fset, []*ast.File{file})
			if gotErr := len(errs) > 0; gotErr != test.wantErr {
				t.Errorf("ungatedMounts errors = %v, want errors: %v", errs, test.wantErr)
			}
		})
	}
}

// mountFailureTests call Mount but fail before anything is mounted.
var mountFailureTests = map[string]string{
	"TestNonexistentMountPoint":        "checkMountPoint rejects the directory",
	"TestMountReturnsTypedFSKitErrors": "the FSKit preflight fails",
}

// mountCallees mount when called: Mount directly, ogletest's RunTests through
// suites that mount in SetUp, and SetUp on samples.SampleTest and
// samples.SubprocessTest.
var mountCallees = []string{"Mount", "RunTests", "SetUp"}

// ungatedMounts checks the test files of one directory. A function or
// package-level variable reaches a mount when its body names a mount callee or
// another one that reaches a mount. A test, benchmark or fuzz target that
// reaches a mount must call SkipUnlessEnabled first. An example cannot skip, so
// an example that reaches a mount must have no output comment, which keeps go
// test from running it. No init may reach a mount, because init runs before any
// gate. ungatedMounts returns one error per violation and the number of entry
// points that reach a mount.
func ungatedMounts(fset *token.FileSet, files []*ast.File) ([]error, int) {
	reaching := map[string]bool{}
	for _, name := range mountCallees {
		reaching[name] = true
	}
	var decls []*ast.FuncDecl
	values := map[string][]ast.Node{}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Body != nil {
					decls = append(decls, decl)
					values[decl.Name.Name] = append(values[decl.Name.Name], decl.Body)
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if spec, ok := spec.(*ast.ValueSpec); ok {
						for i, value := range spec.Values {
							name := spec.Names[i].Name
							values[name] = append(values[name], value)
						}
					}
				}
			}
		}
	}
	for changed := true; changed; {
		changed = false
		for name, nodes := range values {
			for _, node := range nodes {
				if !reaching[name] && references(node, reaching) {
					reaching[name] = true
					changed = true
				}
			}
		}
	}

	runs := map[*ast.BlockStmt]bool{}
	for _, example := range doc.Examples(files...) {
		if body, ok := example.Code.(*ast.BlockStmt); ok && (example.Output != "" || example.EmptyOutput) {
			runs[body] = true
		}
	}

	var errs []error
	checked := 0
	for _, fn := range decls {
		name := fn.Name.Name
		if fn.Recv != nil || !references(fn.Body, reaching) {
			continue
		}
		if _, ok := mountFailureTests[name]; ok {
			continue
		}
		switch {
		case name == "init":
			errs = append(errs, fmt.Errorf("%s: init mounts before any gate can skip; move the mount into a gated test",
				fset.Position(fn.Pos())))
		case isEntry(name, "Test"), isEntry(name, "Benchmark"), isEntry(name, "Fuzz"):
			checked++
			if !gated(fn.Body) {
				errs = append(errs, fmt.Errorf("%s: %s mounts; call mounttest.SkipUnlessEnabled as its first statement",
					fset.Position(fn.Pos()), name))
			}
		case isEntry(name, "Example"):
			checked++
			if runs[fn.Body] {
				errs = append(errs, fmt.Errorf("%s: %s mounts and cannot skip; remove its output comment so go test does not run it",
					fset.Position(fn.Pos()), name))
			}
		}
	}
	return errs, checked
}

// isEntry reports whether name is a go test entry point with prefix, such as
// TestXxx for "Test".
func isEntry(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return rest == "" || !unicode.IsLower(r)
}

// references reports whether node names one of names, as a call or as a
// value such as a subtest function passed to t.Run.
func references(node ast.Node, names map[string]bool) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		if ident, ok := n.(*ast.Ident); ok && names[ident.Name] {
			found = true
		}
		return !found
	})
	return found
}

func gated(body *ast.BlockStmt) bool {
	if len(body.List) == 0 {
		return false
	}
	stmt, ok := body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	call, ok := stmt.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "mounttest" && sel.Sel.Name == "SkipUnlessEnabled"
}
