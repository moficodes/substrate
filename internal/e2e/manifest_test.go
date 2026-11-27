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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

type countingKoBuilder struct {
	calls atomic.Int32
	fn    func(koImageKey) (string, error)
}

func (b *countingKoBuilder) build(key koImageKey) (string, error) {
	b.calls.Add(1)
	return b.fn(key)
}

func TestReplaceKoRefs(t *testing.T) {
	t.Parallel()
	const (
		probeDigest        = "localhost:5001/probe@sha256:1111111111111111111111111111111111111111111111111111111111111111"
		probeNonrootDigest = "localhost:5001/nonroot@sha256:2222222222222222222222222222222222222222222222222222222222222222"
	)

	// One import path is a prefix of the other; greedy token matching keeps the
	// shorter path from corrupting the longer one.
	input := []byte(`containers:
- image: ko://go.ate.dev/substrate/internal/e2e/fixtures/probe/nonroot
- image: "ko://go.ate.dev/substrate/internal/e2e/fixtures/probe"
`)
	var gotUsers []string
	got, err := replaceKoRefs(input, "nonroot:nonroot", func(importPath, imageUser string) (string, error) {
		gotUsers = append(gotUsers, imageUser)
		switch importPath {
		case "go.ate.dev/substrate/internal/e2e/fixtures/probe":
			return probeDigest, nil
		case "go.ate.dev/substrate/internal/e2e/fixtures/probe/nonroot":
			return probeNonrootDigest, nil
		default:
			return "", fmt.Errorf("unexpected import path %q", importPath)
		}
	})
	if err != nil {
		t.Fatalf("replaceKoRefs failed: %v", err)
	}

	want := fmt.Sprintf("containers:\n- image: %s\n- image: %q\n", probeNonrootDigest, probeDigest)
	if string(got) != want {
		t.Fatalf("replaceKoRefs output:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if len(gotUsers) != 2 || gotUsers[0] != "nonroot:nonroot" || gotUsers[1] != "nonroot:nonroot" {
		t.Fatalf("imageUser forwarded = %v, want two nonroot:nonroot calls", gotUsers)
	}
}

func TestKoImageCacheDeduplicatesInProcessAndAcrossProcesses(t *testing.T) {
	t.Parallel()
	sharedDir := t.TempDir()
	builder := &countingKoBuilder{
		fn: func(key koImageKey) (string, error) {
			return fmt.Sprintf("localhost:5001/img:latest@sha256:%064x", len(key.importPath)+len(key.imageUser)), nil
		},
	}

	// Simulate four separate suite binaries sharing E2E_KO_CACHE_DIR, each
	// resolving the same fixture image from eight concurrent goroutines.
	const (
		processes  = 4
		goroutines = 8
	)
	caches := make([]*koImageCache, processes)
	for i := range caches {
		caches[i] = newKoImageCache(sharedDir, builder)
	}

	key := koImageKey{
		importPath: "go.ate.dev/substrate/internal/e2e/fixtures/testserver",
		repo:       "localhost:5001",
		platforms:  "linux/amd64",
	}

	var wg sync.WaitGroup
	errs := make(chan error, processes*goroutines)
	refs := make(chan string, processes*goroutines)
	for _, cache := range caches {
		for range goroutines {
			wg.Add(1)
			go func(c *koImageCache) {
				defer wg.Done()
				ref, err := c.resolve(key)
				if err != nil {
					errs <- err
					return
				}
				refs <- ref
			}(cache)
		}
	}
	wg.Wait()
	close(errs)
	close(refs)

	for err := range errs {
		t.Fatalf("resolve failed: %v", err)
	}
	if got := builder.calls.Load(); got != 1 {
		t.Fatalf("builder calls = %d, want 1 across all processes and goroutines", got)
	}

	var first string
	for ref := range refs {
		if first == "" {
			first = ref
		} else if ref != first {
			t.Fatalf("resolved ref = %q, want %q", ref, first)
		}
	}

	// A distinct imageUser produces a distinct cache key and triggers its own build.
	nonrootKey := key
	nonrootKey.imageUser = "nonroot:nonroot"
	nonrootRef, err := caches[0].resolve(nonrootKey)
	if err != nil {
		t.Fatalf("resolve with imageUser failed: %v", err)
	}
	if nonrootRef == first {
		t.Fatalf("nonroot ref %q matched default ref", nonrootRef)
	}
	if got := builder.calls.Load(); got != 2 {
		t.Fatalf("builder calls after nonroot resolve = %d, want 2", got)
	}
}

func TestKoImageCachePropagatesBuildErrorAndRejectsUnpinnedRef(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		build func(koImageKey) (string, error)
	}{{
		name: "builder error",
		build: func(koImageKey) (string, error) {
			return "", errors.New("boom")
		},
	}, {
		name: "tag without digest",
		build: func(koImageKey) (string, error) {
			return "localhost:5001/probe:latest", nil
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache := newKoImageCache(t.TempDir(), &countingKoBuilder{fn: tt.build})
			_, err := cache.resolve(koImageKey{importPath: "go.ate.dev/substrate/internal/e2e/fixtures/probe"})
			if err == nil {
				t.Fatal("resolve succeeded, want error")
			}
		})
	}
}
