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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"
)

// renderManifest substitutes placeholders into the manifest template at relPath
// (repo-relative), writes the result into the test's temp dir and returns that
// path. Both an apply and a later delete can then consume the same file, with
// no shell involved.
//
// Templates carry two kinds of ${...} placeholder:
//
//   - inline, substituted wherever they appear (an empty value just disappears);
//   - block, which must be the entire content of their line. They expand to a
//     YAML fragment that brings its own indentation, and an empty value takes
//     the whole line with it — the same trick the installer's renderer plays
//     with its drop list. Requiring the placeholder to be the whole line
//     is what lets a comment mention one without being deleted.
func renderManifest(t *testing.T, relPath string, inline, blocks map[string]string) string {
	t.Helper()
	root, err := FindRepoRoot()
	if err != nil {
		t.Fatalf("FindRepoRoot: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, relPath))
	if err != nil {
		t.Fatalf("reading manifest template %s: %v", relPath, err)
	}

	var out []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if value, isBlock := blocks[strings.TrimSpace(line)]; isBlock {
			if value != "" {
				out = append(out, value)
			}
			continue
		}
		for placeholder, value := range inline {
			line = strings.ReplaceAll(line, placeholder, value)
		}
		out = append(out, line)
	}

	rendered := strings.TrimSuffix(filepath.Join(t.TempDir(), filepath.Base(relPath)), ".tmpl")
	if err := os.WriteFile(rendered, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		t.Fatalf("writing rendered manifest %s: %v", rendered, err)
	}
	return rendered
}

// yamlListBlock renders items as `key:` followed by their YAML, every line
// indented by indent spaces, for substitution into a block placeholder. An
// empty list renders to "", which takes the placeholder's whole line — key
// included, since a bare `volumes:` with nothing under it is not what the
// caller meant.
//
// Marshaling the real API types rather than asking callers for YAML text is
// what keeps a fragment honest: a misspelled field is a compile error here,
// where in a template it would apply cleanly and do nothing.
func yamlListBlock[T any](t *testing.T, key string, items []T, indent int) string {
	t.Helper()
	if len(items) == 0 {
		return ""
	}
	raw, err := yaml.Marshal(items)
	if err != nil {
		t.Fatalf("marshaling %s for the manifest: %v", key, err)
	}
	pad := strings.Repeat(" ", indent)
	out := []string{pad + key + ":"}
	for line := range strings.SplitSeq(strings.TrimRight(string(raw), "\n"), "\n") {
		out = append(out, pad+line)
	}
	return strings.Join(out, "\n")
}

// koImagesEnv holds pre-resolved digest-pinned fixture images built once by
// hack/run-e2e.sh, as a YAML stream of "importPath[|imageUser]: image" maps.
const koImagesEnv = "E2E_KO_IMAGES"

// koRefPattern matches a ko:// reference up to the first YAML scalar delimiter.
var koRefPattern = regexp.MustCompile(`ko://[^\s"',\]}]+`)

type koImageKey struct {
	importPath string
	imageUser  string
}

type koImageBuilder interface {
	build(t *testing.T, importPath, imageUser string) string
}

type pinnedKoBuilder struct{}

// build resolves one ko:// import path through hack/run-tool.sh. KO_CONFIG_PATH
// points ko at the repository root's .ko.yaml from the suite's package dir.
func (pinnedKoBuilder) build(t *testing.T, importPath, imageUser string) string {
	t.Helper()
	root, err := FindRepoRoot()
	if err != nil {
		t.Fatalf("FindRepoRoot: %v", err)
	}
	manifest := filepath.Join(t.TempDir(), "fixture-image.yaml")
	if err := os.WriteFile(manifest, []byte("image: ko://"+importPath+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolveArgs := []string{"ko", "resolve"}
	if imageUser != "" {
		resolveArgs = append(resolveArgs, "--image-user", imageUser)
	}
	resolveArgs = append(resolveArgs, "-f", manifest)
	raw := RunCmdOutput(t, []string{"KO_CONFIG_PATH=" + root}, filepath.Join(root, "hack/run-tool.sh"), resolveArgs...)
	doc, _, _ := strings.Cut(string(raw), "\n---")
	var out struct {
		Image string `json:"image"`
	}
	if err := yaml.Unmarshal([]byte(doc), &out); err != nil {
		t.Fatalf("decoding resolved fixture image for %s: %v", importPath, err)
	}
	if !strings.Contains(out.Image, "@sha256:") {
		t.Fatalf("fixture image %q for %s is not pinned by digest", out.Image, importPath)
	}
	return out.Image
}

type koCacheEntry struct {
	once  sync.Once
	image string
}

type koImageCache struct {
	builder koImageBuilder
	entries sync.Map
}

func newKoImageCache(preloaded map[koImageKey]string, builder koImageBuilder) *koImageCache {
	c := &koImageCache{builder: builder}
	for k, image := range preloaded {
		e := &koCacheEntry{image: image}
		e.once.Do(func() {})
		c.entries.Store(k, e)
	}
	return c
}

func (c *koImageCache) resolve(t *testing.T, importPath, imageUser string) string {
	t.Helper()
	key := koImageKey{importPath: importPath, imageUser: imageUser}
	actual, _ := c.entries.LoadOrStore(key, &koCacheEntry{})
	entry := actual.(*koCacheEntry)
	entry.once.Do(func() {
		entry.image = c.builder.build(t, importPath, imageUser)
	})
	if entry.image == "" {
		t.Fatalf("resolving fixture image %s failed in an earlier test", importPath)
	}
	return entry.image
}

func (c *koImageCache) rewriteManifest(t *testing.T, raw []byte, imageUser string) []byte {
	t.Helper()
	var lines []string
	for line := range strings.SplitSeq(string(raw), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			line = koRefPattern.ReplaceAllStringFunc(line, func(match string) string {
				return c.resolve(t, strings.TrimPrefix(match, "ko://"), imageUser)
			})
		}
		lines = append(lines, line)
	}
	return []byte(strings.Join(lines, "\n"))
}

func parseKoImageMap(raw string) (map[koImageKey]string, error) {
	out := make(map[koImageKey]string)
	for doc := range strings.SplitSeq(raw, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var m map[string]string
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", koImagesEnv, err)
		}
		for k, image := range m {
			importPath, imageUser, _ := strings.Cut(k, "|")
			if importPath == "" {
				return nil, fmt.Errorf("%s contains an empty import path key %q", koImagesEnv, k)
			}
			if !strings.Contains(image, "@sha256:") {
				return nil, fmt.Errorf("%s entry %q = %q is not pinned by digest", koImagesEnv, k, image)
			}
			out[koImageKey{importPath: importPath, imageUser: imageUser}] = image
		}
	}
	return out, nil
}

var (
	koImagesOnce  sync.Once
	koImagesCache *koImageCache
	koImagesErr   error
)

func sharedKoImages(t *testing.T) *koImageCache {
	t.Helper()
	koImagesOnce.Do(func() {
		preloaded, err := parseKoImageMap(os.Getenv(koImagesEnv))
		if err != nil {
			koImagesErr = err
			return
		}
		koImagesCache = newKoImageCache(preloaded, pinnedKoBuilder{})
	})
	if koImagesErr != nil {
		t.Fatal(koImagesErr)
	}
	return koImagesCache
}

// ApplyManifest resolves ko:// image references in manifest and applies the
// resulting YAML with kubectl.
func ApplyManifest(t *testing.T, manifest string) {
	t.Helper()
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("reading manifest %s: %v", manifest, err)
	}
	resolved := sharedKoImages(t).rewriteManifest(t, raw, "")
	resolvedPath := filepath.Join(t.TempDir(), filepath.Base(manifest))
	if err := os.WriteFile(resolvedPath, resolved, 0o600); err != nil {
		t.Fatalf("writing resolved manifest %s: %v", resolvedPath, err)
	}
	applyArgs := []string{"apply", "-f", resolvedPath}
	if KubeConfig != "" {
		applyArgs = append([]string{"--kubeconfig=" + KubeConfig}, applyArgs...)
	}
	if KubeContext != "" {
		applyArgs = append([]string{"--context=" + KubeContext}, applyArgs...)
	}
	RunCmd(t, "kubectl", applyArgs...)
}

// koResolve returns the contents of manifest with ko:// image references
// replaced by digest-pinned images. imageUser sets the image USER when
// building a reference that is not already cached.
func koResolve(t *testing.T, manifest, imageUser string) []byte {
	t.Helper()
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("reading manifest %s: %v", manifest, err)
	}
	return sharedKoImages(t).rewriteManifest(t, raw, imageUser)
}

// ResolveFixtureImage resolves importPath through the fixture image cache and
// returns its digest-pinned image reference.
func ResolveFixtureImage(t *testing.T, importPath string) string {
	t.Helper()
	return sharedKoImages(t).resolve(t, importPath, "")
}
