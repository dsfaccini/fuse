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

func TestGatesSkipByDefault(t *testing.T) {
	for _, gate := range []struct {
		env  string
		skip func(testing.TB)
	}{
		{Env, SkipUnlessEnabled},
		{FSKitProbeEnv, SkipUnlessFSKitProbeEnabled},
	} {
		t.Setenv(Env, "")
		t.Setenv(FSKitProbeEnv, "")
		reached := false
		t.Run(gate.env, func(t *testing.T) {
			gate.skip(t)
			reached = true
		})
		if reached {
			t.Errorf("the %s gate did not skip with it unset", gate.env)
		}
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
		name     string
		src      string
		wantErrs int
	}{
		{
			name: "helper",
			src: `package p
func mountMinimalFS(t *testing.T) { fuse.Mount("", nil, nil) }
func TestMountViaHelper(t *testing.T) { mountMinimalFS(t) }`,
			wantErrs: 1,
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
			wantErrs: 1,
		},
		{
			name: "example without output",
			src: `package p
func ExampleMount() { fuse.Mount("", nil, nil) }`,
		},
		{
			name: "FSKit probe gate",
			src: `package p
func runProbe(t *testing.T) { mountFSKitTransport("", nil, nil) }
func TestProbe(t *testing.T) {
	mounttest.SkipUnlessFSKitProbeEnabled(t)
	runProbe(t)
}`,
		},
		{
			name: "ungated FSKit transport",
			src: `package p
func TestProbe(t *testing.T) { mountFSKitTransport("", nil, nil) }`,
			wantErrs: 1,
		},
		{
			name: "FSKit transport behind the mount-test gate",
			src: `package p
func TestProbe(t *testing.T) {
	mounttest.SkipUnlessEnabled(t)
	mountFSKitTransport("", nil, nil)
}`,
			wantErrs: 1,
		},
		{
			name: "allowlisted test reaching an FSKit mount",
			src: `package p
func TestMountReturnsTypedFSKitErrors(t *testing.T) { mountFSKitTransport("", nil, nil) }`,
			wantErrs: 1,
		},
		{
			name: "mount behind the FSKit probe gate",
			src: `package p
func TestMount(t *testing.T) {
	mounttest.SkipUnlessFSKitProbeEnabled(t)
	fuse.Mount("", nil, nil)
}`,
			wantErrs: 1,
		},
		{
			name: "direct SetUp",
			src: `package p
func TestHelloSetUpDirect(t *testing.T) { (&samples.SampleTest{}).SetUp(nil) }`,
			wantErrs: 1,
		},
		{
			name: "fuzz target",
			src: `package p
func FuzzMount(f *testing.F) { f.Fuzz(func(t *testing.T, dir string) { fuse.Mount(dir, nil, nil) }) }`,
			wantErrs: 1,
		},
		{
			name: "named subtest",
			src: `package p
func mountSub(t *testing.T) { fuse.Mount("", nil, nil) }
func TestNamedSubtest(t *testing.T) { t.Run("mount", mountSub) }`,
			wantErrs: 1,
		},
		{
			name: "named fuzz function",
			src: `package p
func fuzzMount(t *testing.T, dir string) { fuse.Mount(dir, nil, nil) }
func FuzzNamed(f *testing.F) { f.Fuzz(fuzzMount) }`,
			wantErrs: 1,
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
			wantErrs: 1,
		},
		{
			name: "package-level func var",
			src: `package p
var mountVar = func(t *testing.T) { fuse.Mount("", nil, nil) }
func TestFuncVar(t *testing.T) { mountVar(t) }`,
			wantErrs: 1,
		},
		{
			name: "init",
			src: `package p
func init() { fuse.Mount("", nil, nil) }`,
			wantErrs: 1,
		},
		{
			name: "whole-file example",
			src: `package p
type exampleFS struct{}
func ExampleMount() {
	fuse.Mount("", nil, nil)
	// Output:
}`,
			wantErrs: 1,
		},
		{
			name: "multi-name var",
			src: `package p
var mountA, mountB = mountPair()
func mountPair() (func(*testing.T), func(*testing.T)) {
	return func(t *testing.T) { fuse.Mount("", nil, nil) }, nil
}
func TestSecondName(t *testing.T) { mountB(t) }`,
			wantErrs: 1,
		},
		{
			name: "package-level initializer",
			src: `package p
var fixture = mountAtInit()
func mountAtInit() int { fuse.Mount("", nil, nil); return 0 }`,
			wantErrs: 1,
		},
		{
			name: "blank initializer",
			src: `package p
var _ = mountAtInit()
func mountAtInit() int { fuse.Mount("", nil, nil); return 0 }
func TestUnrelated(t *testing.T) { _ = 1 }`,
			wantErrs: 1,
		},
		{
			name: "package-level func literal",
			src: `package p
var mountLater = func(t *testing.T) { fuse.Mount("", nil, nil) }`,
		},
		{
			name: "init storing a mounting function",
			src: `package p
var subtests []func(*testing.T)
func mountSub(t *testing.T) { fuse.Mount("", nil, nil) }
func init() { subtests = append(subtests, mountSub) }`,
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
			if len(errs) != test.wantErrs {
				t.Errorf("ungatedMounts errors = %v, want %d", errs, test.wantErrs)
			}
		})
	}
}

// mountFailureTests call Mount but fail before anything is mounted. A test
// that reaches an FSKit mount cannot be listed.
var mountFailureTests = map[string]string{
	"TestNonexistentMountPoint":                   "checkMountPoint rejects the directory",
	"TestMountReturnsTypedFSKitErrors":            "the FSKit preflight fails",
	"TestMountRejectsInvalidConfigBeforeMounting": "config validation fails",
}

// mountCallees mount when called: Mount directly, mountFSKitTransport under
// the FSKit probe, ogletest's RunTests through suites that mount in SetUp, and
// SetUp on samples.SampleTest and samples.SubprocessTest.
var mountCallees = []string{"Mount", "mountFSKitTransport", "RunTests", "SetUp"}

// fskitMountCallees mount through FSKit. Only the FSKit probe's gate may guard
// them, so that FUSE_RUN_MOUNT_TESTS never starts an FSKit mount.
var fskitMountCallees = []string{"mountFSKitTransport"}

// ungatedMounts checks the test files of one directory. A function or
// package-level variable reaches a mount when its body names a mount callee or
// another one that reaches a mount. A test, benchmark or fuzz target that
// reaches a mount must call SkipUnlessEnabled first, or
// SkipUnlessFSKitProbeEnabled when it reaches an FSKit mount. An example cannot skip, so
// an example that reaches a mount must have no output comment, which keeps go
// test from running it. No init and no package-level initializer may call a
// function that mounts, because both run before any gate. ungatedMounts returns one error per
// violation and the number of entry points that reach a mount.
func ungatedMounts(fset *token.FileSet, files []*ast.File) ([]error, int) {
	var decls []*ast.FuncDecl
	var initializers []ast.Expr
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
					spec, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					initializers = append(initializers, spec.Values...)
					for i, ident := range spec.Names {
						if ident.Name == "_" || len(spec.Values) == 0 {
							continue
						}
						// var a, b = f() gives both names the one value.
						value := spec.Values[0]
						if len(spec.Values) == len(spec.Names) {
							value = spec.Values[i]
						}
						values[ident.Name] = append(values[ident.Name], value)
					}
				}
			}
		}
	}
	reach := func(callees []string) map[string]bool {
		reaching := map[string]bool{}
		for _, name := range callees {
			reaching[name] = true
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
		return reaching
	}
	reaching := reach(mountCallees)
	reachingFSKit := reach(fskitMountCallees)

	// calling holds the functions that mount when called. A function literal
	// inside a body does not run when the body runs.
	calling := map[string]bool{}
	for _, name := range mountCallees {
		calling[name] = true
	}
	for changed := true; changed; {
		changed = false
		for _, fn := range decls {
			if !calling[fn.Name.Name] && callsOutsideFuncLit(fn.Body, calling) {
				calling[fn.Name.Name] = true
				changed = true
			}
		}
	}

	// Code is the whole file for a file that holds one example and no tests,
	// so key by name.
	runs := map[string]bool{}
	for _, example := range doc.Examples(files...) {
		if example.Output != "" || example.EmptyOutput {
			runs["Example"+example.Name] = true
		}
	}

	var errs []error
	for _, value := range initializers {
		if callsOutsideFuncLit(value, calling) {
			errs = append(errs, fmt.Errorf("%s: a package-level initializer mounts before any gate can skip; move the mount into a gated test",
				fset.Position(value.Pos())))
		}
	}
	checked := 0
	for _, fn := range decls {
		name := fn.Name.Name
		if fn.Recv != nil {
			continue
		}
		if name == "init" {
			if callsOutsideFuncLit(fn.Body, calling) {
				errs = append(errs, fmt.Errorf("%s: init mounts before any gate can skip; move the mount into a gated test",
					fset.Position(fn.Pos())))
			}
			continue
		}
		if !references(fn.Body, reaching) {
			continue
		}
		fskit := references(fn.Body, reachingFSKit)
		if _, ok := mountFailureTests[name]; ok {
			if fskit {
				errs = append(errs, fmt.Errorf("%s: %s reaches an FSKit mount, which mountFailureTests cannot exempt; call mounttest.SkipUnlessFSKitProbeEnabled as its first statement",
					fset.Position(fn.Pos()), name))
			}
			continue
		}
		switch {
		case isEntry(name, "Test"), isEntry(name, "Benchmark"), isEntry(name, "Fuzz"):
			checked++
			switch {
			case fskit && !gated(fn.Body, "SkipUnlessFSKitProbeEnabled"):
				errs = append(errs, fmt.Errorf("%s: %s mounts through FSKit; call mounttest.SkipUnlessFSKitProbeEnabled as its first statement",
					fset.Position(fn.Pos()), name))
			case !fskit && !gated(fn.Body, "SkipUnlessEnabled"):
				errs = append(errs, fmt.Errorf("%s: %s mounts; call mounttest.SkipUnlessEnabled as its first statement, or list it in mountFailureTests if it fails before anything is mounted",
					fset.Position(fn.Pos()), name))
			}
		case isEntry(name, "Example"):
			checked++
			if runs[name] {
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

// callsOutsideFuncLit reports whether running node calls one of names. A
// function literal in node does not run when node runs.
func callsOutsideFuncLit(node ast.Node, names map[string]bool) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			if names[calleeName(n)] {
				found = true
			}
		}
		return !found
	})
	return found
}

// gated reports whether body starts with a call to mounttest's gate function.
func gated(body *ast.BlockStmt, gate string) bool {
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
	return ok && pkg.Name == "mounttest" && sel.Sel.Name == gate
}

func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}
