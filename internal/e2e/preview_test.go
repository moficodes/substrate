// Copyright 2026 The Agent Substrate Authors
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

package e2e

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	"go.ate.dev/substrate/internal/preview"
)

func TestSnapshotLocation(t *testing.T) {
	t.Setenv("BUCKET_NAME", "test-bucket")
	for _, tc := range []struct {
		name  string
		gates []string
		want  string
	}{
		{name: "gate off"},
		{name: "gate on", gates: []string{string(preview.GatePerTemplateSnapshotLocation)}, want: "gs://test-bucket/ate-e2e-probe/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preview.InitForTest(t, tc.gates...)
			if got := SnapshotLocation(t, "ate-e2e-probe"); got != tc.want {
				t.Errorf("SnapshotLocation() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPreviewGates(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ci      string
		value   *string
		want    []string
		wantErr bool
	}{
		{name: "unset locally"},
		{name: "unset in CI", ci: "true", wantErr: true},
		{name: "empty in CI", ci: "true", value: ptr.To("")},
		{name: "ALL", ci: "true", value: ptr.To("ALL"), want: []string{"ALL"}},
		{name: "list", value: ptr.To("A, B C"), want: []string{"A", "B", "C"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CI", tc.ci)

			got, err := previewGates(ptr.Deref(tc.value, ""), tc.value != nil)
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("previewGates() error = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && !slices.Equal(got, tc.want) {
				t.Errorf("previewGates() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPreviewOnlyFilter checks that preview_target_paths and preview_tests in
// hack/run-e2e.sh match the suites and top-level tests under internal/e2e/suites
// that call RequirePreview, RequireIngressCache, or preview.IsEnabled, and that
// none of those tests read EgressFixture (which the preview CI lane skips
// deploying).
func TestPreviewOnlyFilter(t *testing.T) {
	raw, err := os.ReadFile("../../hack/run-e2e.sh")
	if err != nil {
		t.Fatal(err)
	}
	gotSuites := parseBashArray(t, string(raw), "preview_target_paths")
	gotTests := parseBashArray(t, string(raw), "preview_tests")
	if !slices.IsSorted(gotSuites) {
		t.Errorf("preview_target_paths in hack/run-e2e.sh must stay sorted: %v", gotSuites)
	}
	if !slices.IsSorted(gotTests) {
		t.Errorf("preview_tests in hack/run-e2e.sh must stay sorted: %v", gotTests)
	}

	entries, err := os.ReadDir("suites")
	if err != nil {
		t.Fatal(err)
	}
	var wantSuites, wantTests []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		tests, egressUsers := discoverPreviewSuiteTests(t, filepath.Join("suites", entry.Name()))
		if len(tests) > 0 {
			wantSuites = append(wantSuites, "./internal/e2e/suites/"+entry.Name())
			wantTests = append(wantTests, tests...)
		}
		for _, testName := range egressUsers {
			t.Errorf("preview test %s in suites/%s calls e2e.EgressFixture(), which pr-workflow.yaml skips deploying when preview=='on'", testName, entry.Name())
		}
	}
	slices.Sort(wantSuites)
	slices.Sort(wantTests)

	if !slices.Equal(gotSuites, wantSuites) {
		t.Errorf("preview_target_paths in hack/run-e2e.sh = %v, want %v", gotSuites, wantSuites)
	}
	if !slices.Equal(gotTests, wantTests) {
		t.Errorf("preview_tests in hack/run-e2e.sh = %v, want %v", gotTests, wantTests)
	}
}

func parseBashArray(t *testing.T, script, name string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(script, name+"=(\n")
	if !ok {
		t.Fatalf("array %s not found in hack/run-e2e.sh", name)
	}
	body, _, ok := strings.Cut(rest, "\n)")
	if !ok {
		t.Fatalf("closing paren for %s not found in hack/run-e2e.sh", name)
	}
	var out []string
	for line := range strings.SplitSeq(body, "\n") {
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = line[:idx]
		}
		if item := strings.TrimSpace(line); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func discoverPreviewSuiteTests(t *testing.T, dir string) (previewTests, egressUsers []string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	calls := map[string][]string{}
	directPreview := map[string]bool{}
	directEgress := map[string]bool{}
	var topTests []string

	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if strings.HasPrefix(name, "Test") && name != "TestMain" {
				topTests = append(topTests, name)
			}
			if skipsWhenPreviewEnabled(fn) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					calls[name] = append(calls[name], fun.Name)
				case *ast.SelectorExpr:
					pkg, ok := fun.X.(*ast.Ident)
					if !ok {
						return true
					}
					if pkg.Name == "e2e" && (fun.Sel.Name == "RequirePreview" || fun.Sel.Name == "RequireIngressCache") {
						directPreview[name] = true
					}
					if pkg.Name == "preview" && fun.Sel.Name == "IsEnabled" {
						directPreview[name] = true
					}
					if pkg.Name == "e2e" && fun.Sel.Name == "EgressFixture" {
						directEgress[name] = true
					}
				}
				return true
			})
		}
	}

	reaches := func(start string, target map[string]bool) bool {
		seen := map[string]bool{}
		var walk func(string) bool
		walk = func(fn string) bool {
			if target[fn] {
				return true
			}
			if seen[fn] {
				return false
			}
			seen[fn] = true
			for _, next := range calls[fn] {
				if walk(next) {
					return true
				}
			}
			return false
		}
		return walk(start)
	}

	for _, name := range topTests {
		if reaches(name, directPreview) {
			previewTests = append(previewTests, name)
			if reaches(name, directEgress) {
				egressUsers = append(egressUsers, name)
			}
		}
	}
	return previewTests, egressUsers
}

// skipsWhenPreviewEnabled reports whether fn starts with `if preview.IsEnabled(...) { t.Skip... }`,
// which marks a test covering the default gates-off behavior.
func skipsWhenPreviewEnabled(fn *ast.FuncDecl) bool {
	if fn.Body == nil || len(fn.Body.List) == 0 {
		return false
	}
	ifStmt, ok := fn.Body.List[0].(*ast.IfStmt)
	if !ok || ifStmt.Body == nil || len(ifStmt.Body.List) != 1 {
		return false
	}
	cond, ok := ifStmt.Cond.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := cond.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "IsEnabled" {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "preview" {
		return false
	}
	expr, ok := ifStmt.Body.List[0].(*ast.ExprStmt)
	if !ok {
		return false
	}
	skipCall, ok := expr.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	skipSel, ok := skipCall.Fun.(*ast.SelectorExpr)
	return ok && (skipSel.Sel.Name == "Skip" || skipSel.Sel.Name == "Skipf")
}
