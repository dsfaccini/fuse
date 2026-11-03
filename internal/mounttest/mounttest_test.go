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
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// mountFailureTests call Mount but fail before anything is mounted.
var mountFailureTests = map[string]string{
	"TestNonexistentMountPoint":        "checkMountPoint rejects the directory",
	"TestMountReturnsTypedFSKitErrors": "the FSKit preflight fails",
}

// TestMountingTestsAreGated scans every test in the module. A test that calls
// Mount, or ogletest's RunTests whose suites mount in SetUp, must call
// SkipUnlessEnabled first.
func TestMountingTestsAreGated(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root: %v", err)
	}

	fset := token.NewFileSet()
	checked := 0
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
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") {
				continue
			}
			if _, ok := mountFailureTests[fn.Name.Name]; ok || !mounts(fn.Body) {
				continue
			}
			checked++
			if !gated(fn.Body) {
				t.Errorf("%s: %s mounts; call mounttest.SkipUnlessEnabled(t) as its first statement",
					fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("found no mounting tests; the scan is broken")
	}
}

func mounts(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			switch calleeName(call) {
			case "Mount", "RunTests":
				found = true
			}
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

func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}
