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
	"regexp"
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

// TestPreviewWorkflowFilter checks that PREVIEW_RUN_REGEX in
// .github/workflows/pr-workflow.yaml matches every e2e test that checks a
// preview gate, carries no dead branches, and matches no test that depends on
// EgressFixture (which preview lanes skip deploying).
func TestPreviewWorkflowFilter(t *testing.T) {
	workflowPath := filepath.Join("..", "..", ".github", "workflows", "pr-workflow.yaml")
	workflowBytes, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("reading %s: %v", workflowPath, err)
	}
	lineRe := regexp.MustCompile(`PREVIEW_RUN_REGEX:\s*\$\{\{\s*matrix\.preview\s*==\s*'on'\s*&&\s*'([^']+)'`)
	m := lineRe.FindSubmatch(workflowBytes)
	if m == nil {
		t.Fatalf("PREVIEW_RUN_REGEX expression missing from %s", workflowPath)
	}
	pattern := string(m[1])
	runRe, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("invalid PREVIEW_RUN_REGEX %q in %s: %v", pattern, workflowPath, err)
	}
	if !strings.HasPrefix(pattern, "^(") || !strings.HasSuffix(pattern, ")$") {
		t.Fatalf("PREVIEW_RUN_REGEX %q must have the anchored shape ^(branch1|branch2|...)$", pattern)
	}
	branches := strings.Split(strings.TrimSuffix(strings.TrimPrefix(pattern, "^("), ")$"), "|")
	branchRes := make([]*regexp.Regexp, len(branches))
	for i, b := range branches {
		branchRes[i], err = regexp.Compile("^(" + b + ")$")
		if err != nil {
			t.Fatalf("invalid branch %q in PREVIEW_RUN_REGEX: %v", b, err)
		}
	}

	suiteEntries, err := os.ReadDir("suites")
	if err != nil {
		t.Fatalf("reading suites directory: %v", err)
	}

	branchMatched := make([]bool, len(branches))
	for _, entry := range suiteEntries {
		if !entry.IsDir() {
			continue
		}
		suiteDir := filepath.Join("suites", entry.Name())
		files, err := filepath.Glob(filepath.Join(suiteDir, "*_test.go"))
		if err != nil {
			t.Fatalf("globbing %s: %v", suiteDir, err)
		}

		fset := token.NewFileSet()
		calls := map[string][]string{}
		usesPreview := map[string]bool{}
		usesEgressDemo := map[string]bool{}
		var testFuncs []string

		for _, file := range files {
			f, err := parser.ParseFile(fset, file, nil, 0)
			if err != nil {
				t.Fatalf("parsing %s: %v", file, err)
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv != nil || fn.Body == nil {
					continue
				}
				name := fn.Name.Name
				if strings.HasPrefix(name, "Test") && name != "TestMain" {
					testFuncs = append(testFuncs, name)
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.Ident:
						calls[name] = append(calls[name], x.Name)
					case *ast.SelectorExpr:
						pkgIdent, ok := x.X.(*ast.Ident)
						if !ok {
							return true
						}
						if pkgIdent.Name == "e2e" && (x.Sel.Name == "RequirePreview" || x.Sel.Name == "RequireIngressCache") {
							usesPreview[name] = true
						}
						if pkgIdent.Name == "preview" && x.Sel.Name == "IsEnabled" {
							usesPreview[name] = true
						}
						if pkgIdent.Name == "e2e" && x.Sel.Name == "EgressFixture" {
							usesEgressDemo[name] = true
						}
					}
					return true
				})
			}
		}

		propagateCallGraph(calls, usesPreview)
		propagateCallGraph(calls, usesEgressDemo)

		for _, testName := range testFuncs {
			matched := runRe.MatchString(testName)
			if usesPreview[testName] && !matched {
				t.Errorf("%s/%s checks a preview gate; add it to PREVIEW_RUN_REGEX in .github/workflows/pr-workflow.yaml", suiteDir, testName)
			}
			if matched && usesEgressDemo[testName] {
				t.Errorf("%s/%s matches PREVIEW_RUN_REGEX and calls e2e.EgressFixture(), which preview lanes skip deploying", suiteDir, testName)
			}
			for i, br := range branchRes {
				if br.MatchString(testName) {
					branchMatched[i] = true
				}
			}
		}
	}

	for i, b := range branches {
		if !branchMatched[i] {
			t.Errorf("PREVIEW_RUN_REGEX branch %q matched no test in internal/e2e/suites", b)
		}
	}
}

func propagateCallGraph(calls map[string][]string, marked map[string]bool) {
	for changed := true; changed; {
		changed = false
		for caller, callees := range calls {
			if marked[caller] {
				continue
			}
			for _, callee := range callees {
				if marked[callee] {
					marked[caller] = true
					changed = true
					break
				}
			}
		}
	}
}
