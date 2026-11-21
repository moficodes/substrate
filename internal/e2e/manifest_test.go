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
	"maps"
	"strings"
	"sync"
	"testing"
)

const testDigest = "@sha256:1111111111111111111111111111111111111111111111111111111111111111"

type stubKoBuilder struct {
	mu    sync.Mutex
	calls map[koImageKey]int
}

func newStubKoBuilder() *stubKoBuilder {
	return &stubKoBuilder{calls: make(map[koImageKey]int)}
}

func (s *stubKoBuilder) build(_ *testing.T, importPath, imageUser string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := koImageKey{importPath: importPath, imageUser: imageUser}
	s.calls[key]++
	tag := "latest"
	if imageUser != "" {
		tag = strings.ReplaceAll(imageUser, ":", "-")
	}
	return "localhost:5001/" + importPath + ":" + tag + testDigest
}

func TestParseKoImageMap(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    map[koImageKey]string
		wantErr string
	}{{
		name: "empty",
		raw:  "",
		want: map[koImageKey]string{},
	}, {
		name: "multi-document stream with default and user-scoped keys",
		raw: "go.ate.dev/substrate/internal/e2e/fixtures/probe: localhost:5001/probe:latest" + testDigest + "\n\n---\n" +
			"\"go.ate.dev/substrate/internal/e2e/fixtures/probe/nonroot|nonroot:nonroot\": localhost:5001/nonroot:latest" + testDigest + "\n\n---\n",
		want: map[koImageKey]string{
			{importPath: "go.ate.dev/substrate/internal/e2e/fixtures/probe"}:                                       "localhost:5001/probe:latest" + testDigest,
			{importPath: "go.ate.dev/substrate/internal/e2e/fixtures/probe/nonroot", imageUser: "nonroot:nonroot"}: "localhost:5001/nonroot:latest" + testDigest,
		},
	}, {
		name:    "unpinned reference",
		raw:     "go.ate.dev/substrate/internal/e2e/fixtures/probe: localhost:5001/probe:latest\n",
		wantErr: "is not pinned by digest",
	}, {
		name:    "invalid yaml",
		raw:     "- not-a-map\n",
		wantErr: "parsing E2E_KO_IMAGES",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseKoImageMap(tt.raw)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseKoImageMap() error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseKoImageMap() unexpected error: %v", err)
			}
			if !maps.Equal(got, tt.want) {
				t.Errorf("parseKoImageMap() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKoImageCacheRewriteManifest(t *testing.T) {
	const (
		probePkg      = "go.ate.dev/substrate/internal/e2e/fixtures/probe"
		nonrootPkg    = "go.ate.dev/substrate/internal/e2e/fixtures/probe/nonroot"
		testserverPkg = "go.ate.dev/substrate/internal/e2e/fixtures/testserver"
		preloadedRef  = "localhost:5001/preloaded-probe:latest" + testDigest
	)

	builder := newStubKoBuilder()
	cache := newKoImageCache(map[koImageKey]string{
		{importPath: probePkg}: preloadedRef,
	}, builder)

	in := strings.Join([]string{
		"# ko://go.ate.dev/substrate/commented/out",
		"image: ko://" + probePkg,
		`other: "ko://` + testserverPkg + `"`,
	}, "\n")

	for range 3 {
		got := string(cache.rewriteManifest(t, []byte(in), ""))
		want := strings.Join([]string{
			"# ko://go.ate.dev/substrate/commented/out",
			"image: " + preloadedRef,
			`other: "localhost:5001/` + testserverPkg + `:latest` + testDigest + `"`,
		}, "\n")
		if got != want {
			t.Fatalf("rewriteManifest() =\n%s\nwant\n%s", got, want)
		}
	}

	gotNonroot := string(cache.rewriteManifest(t, []byte("image: ko://"+nonrootPkg), "nonroot:nonroot"))
	wantNonroot := "image: localhost:5001/" + nonrootPkg + ":nonroot-nonroot" + testDigest
	if gotNonroot != wantNonroot {
		t.Fatalf("rewriteManifest(nonroot) = %q, want %q", gotNonroot, wantNonroot)
	}

	wantCalls := map[koImageKey]int{
		{importPath: testserverPkg}:                            1,
		{importPath: nonrootPkg, imageUser: "nonroot:nonroot"}: 1,
	}
	if !maps.Equal(builder.calls, wantCalls) {
		t.Errorf("builder calls = %v, want %v", builder.calls, wantCalls)
	}
}
