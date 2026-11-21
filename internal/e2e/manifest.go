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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"

	"sigs.k8s.io/yaml"
)

// koCacheDirEnv names a directory shared across the suite binaries of one
// hack/run-e2e.sh invocation so each fixture image is built and pushed once per
// run. When unset, resolved digests are still cached within the current test
// binary.
const koCacheDirEnv = "E2E_KO_CACHE_DIR"

// koRefPattern matches ko:// Go import paths inside YAML or JSON manifests.
var koRefPattern = regexp.MustCompile(`ko://[A-Za-z0-9._/-]+`)

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

// koImageKey identifies one ko-built fixture image variant.
type koImageKey struct {
	importPath string
	imageUser  string
	repo       string
	platforms  string
}

func (k koImageKey) cacheName() string {
	raw := strings.Join([]string{k.importPath, k.imageUser, k.repo, k.platforms}, "\x00")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}

// koBuilder builds and pushes one ko image and returns its digest-pinned
// reference.
type koBuilder interface {
	build(key koImageKey) (string, error)
}

type realKoBuilder struct{}

// build invokes the repository's pinned ko binary (through hack/run-tool.sh)
// with KO_CONFIG_PATH set to the repo root so .ko.yaml's baseImageOverrides and
// defaultPlatforms always apply regardless of the suite's working directory.
func (realKoBuilder) build(key koImageKey) (string, error) {
	root, err := FindRepoRoot()
	if err != nil {
		return "", fmt.Errorf("FindRepoRoot: %w", err)
	}
	args := []string{"ko", "build"}
	if key.imageUser != "" {
		args = append(args, "--image-user="+key.imageUser)
	}
	args = append(args, key.importPath)

	cmd := exec.Command(filepath.Join(root, "hack/run-tool.sh"), args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "KO_CONFIG_PATH="+root)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderrColor := &ColorWriter{W: os.Stderr, ANSI: ansiRed}
	cmd.Stderr = NewIndentWriter(stderrColor, "        ")
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("running %s: %w", strings.Join(args, " "), err)
	}
	fields := strings.Fields(stdout.String())
	if len(fields) == 0 {
		return "", fmt.Errorf("ko build %s printed no image reference", key.importPath)
	}
	ref := fields[len(fields)-1]
	if !strings.Contains(ref, "@sha256:") {
		return "", fmt.Errorf("fixture image %q is not pinned by digest", ref)
	}
	return ref, nil
}

type koImageEntry struct {
	once  sync.Once
	image string
	err   error
}

type koImageCache struct {
	mu       sync.Mutex
	entries  map[koImageKey]*koImageEntry
	cacheDir string
	builder  koBuilder
}

func newKoImageCache(cacheDir string, builder koBuilder) *koImageCache {
	return &koImageCache{
		entries:  make(map[koImageKey]*koImageEntry),
		cacheDir: cacheDir,
		builder:  builder,
	}
}

var defaultKoImageCache = newKoImageCache("", realKoBuilder{})

func (c *koImageCache) resolve(key koImageKey) (string, error) {
	c.mu.Lock()
	entry, ok := c.entries[key]
	if !ok {
		entry = &koImageEntry{}
		c.entries[key] = entry
	}
	c.mu.Unlock()

	entry.once.Do(func() {
		entry.image, entry.err = c.resolveShared(key)
	})
	return entry.image, entry.err
}

func (c *koImageCache) resolveShared(key koImageKey) (string, error) {
	cacheDir := c.cacheDir
	if cacheDir == "" {
		cacheDir = os.Getenv(koCacheDirEnv)
	}
	if cacheDir == "" {
		return c.buildAndValidate(key)
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return "", fmt.Errorf("creating ko cache dir %s: %w", cacheDir, err)
	}

	name := key.cacheName()
	refPath := filepath.Join(cacheDir, name+".ref")
	if ref, ok := readCachedImageRef(refPath); ok {
		return ref, nil
	}

	lockPath := filepath.Join(cacheDir, name+".lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", fmt.Errorf("opening ko cache lock %s: %w", lockPath, err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("locking ko cache %s: %w", lockPath, err)
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	if ref, ok := readCachedImageRef(refPath); ok {
		return ref, nil
	}

	ref, err := c.buildAndValidate(key)
	if err != nil {
		return "", err
	}
	if err := writeCachedImageRef(cacheDir, name, refPath, ref); err != nil {
		return "", err
	}
	return ref, nil
}

func (c *koImageCache) buildAndValidate(key koImageKey) (string, error) {
	ref, err := c.builder.build(key)
	if err != nil {
		return "", err
	}
	if !strings.Contains(ref, "@sha256:") {
		return "", fmt.Errorf("fixture image %q is not pinned by digest", ref)
	}
	return ref, nil
}

func readCachedImageRef(refPath string) (string, bool) {
	raw, err := os.ReadFile(refPath)
	if err != nil {
		return "", false
	}
	ref := strings.TrimSpace(string(raw))
	if !strings.Contains(ref, "@sha256:") {
		return "", false
	}
	return ref, true
}

func writeCachedImageRef(cacheDir, name, refPath, ref string) error {
	tmp, err := os.CreateTemp(cacheDir, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp ko cache entry in %s: %w", cacheDir, err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(ref + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing ko cache entry %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing ko cache entry %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, refPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("publishing ko cache entry %s: %w", refPath, err)
	}
	return nil
}

func resolveFixtureImage(t *testing.T, importPath, imageUser string) string {
	t.Helper()
	key := koImageKey{
		importPath: importPath,
		imageUser:  imageUser,
		repo:       os.Getenv("KO_DOCKER_REPO"),
		platforms:  os.Getenv("KO_DEFAULTPLATFORMS"),
	}
	ref, err := defaultKoImageCache.resolve(key)
	if err != nil {
		t.Fatalf("resolving ko://%s: %v", importPath, err)
	}
	return ref
}

func replaceKoRefs(raw []byte, imageUser string, resolve func(importPath, imageUser string) (string, error)) ([]byte, error) {
	var firstErr error
	replaced := koRefPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		if firstErr != nil {
			return match
		}
		importPath := strings.TrimPrefix(string(match), "ko://")
		ref, err := resolve(importPath, imageUser)
		if err != nil {
			firstErr = err
			return match
		}
		return []byte(ref)
	})
	if firstErr != nil {
		return nil, firstErr
	}
	return replaced, nil
}

// koApply resolves the ko:// images named in manifest and applies the result
// with kubectl.
func koApply(t *testing.T, manifest string) {
	t.Helper()
	resolved := koResolve(t, manifest, "")
	applied := filepath.Join(t.TempDir(), "applied-"+filepath.Base(manifest))
	if err := os.WriteFile(applied, resolved, 0o600); err != nil {
		t.Fatalf("writing resolved manifest %s: %v", applied, err)
	}
	applyArgs := []string{"apply", "-f", applied}
	if KubeContext != "" {
		applyArgs = append([]string{"--context=" + KubeContext}, applyArgs...)
	}
	RunCmd(t, "kubectl", applyArgs...)
}

// koResolve resolves the ko:// images named in manifest (building with
// imageUser when non-empty) and returns the manifest with those references
// replaced by pushed digests.
func koResolve(t *testing.T, manifest, imageUser string) []byte {
	t.Helper()
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("reading manifest %s: %v", manifest, err)
	}
	out, err := replaceKoRefs(raw, imageUser, func(importPath, user string) (string, error) {
		return resolveFixtureImage(t, importPath, user), nil
	})
	if err != nil {
		t.Fatalf("resolving manifest %s: %v", manifest, err)
	}
	return out
}

// ResolveFixtureImage builds a fixture through the shared ko cache and returns
// its digest-pinned image reference.
func ResolveFixtureImage(t *testing.T, importPath string) string {
	t.Helper()
	return resolveFixtureImage(t, importPath, "")
}
