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

package images

import (
	"archive/tar"
	"bytes"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func layerFromContent(t *testing.T, name, body string) v1.Layer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

func imageWithLayers(t *testing.T, osName, arch string, layers ...v1.Layer) v1.Image {
	t.Helper()
	cfg, err := empty.Image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cfg.OS = osName
	cfg.Architecture = arch
	img, err := mutate.ConfigFile(empty.Image, cfg)
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.AppendLayers(img, layers...)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// Archiving from one registry and pushing to a second must preserve every
// manifest digest and platform while storing shared layer blobs once on disk.
func TestArchiveRoundTripPreservesDigestsAndDeduplicatesSharedLayers(t *testing.T) {
	srcSrv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srcSrv.Close)
	srcRepo := strings.TrimPrefix(srcSrv.URL, "http://")

	dstSrv := httptest.NewServer(registry.New(registry.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(dstSrv.Close)
	dstRepo := strings.TrimPrefix(dstSrv.URL, "http://") + "/substrate"

	sharedBase := layerFromContent(t, "base.txt", strings.Repeat("shared-base-", 256))
	sharedDigest, err := sharedBase.Digest()
	if err != nil {
		t.Fatal(err)
	}
	ateapiImg := imageWithLayers(t, "linux", "amd64", sharedBase, layerFromContent(t, "ko-app/ateapi", "ateapi-bin"))
	ateletImg := imageWithLayers(t, "linux", "amd64", sharedBase, layerFromContent(t, "ko-app/atelet", "atelet-bin"))
	if err := remote.Write(parseRef(t, srcRepo+"/ateapi:stage"), ateapiImg); err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(parseRef(t, srcRepo+"/atelet:stage"), ateletImg); err != nil {
		t.Fatal(err)
	}

	amd64 := v1.Platform{OS: "linux", Architecture: "amd64"}
	arm64 := v1.Platform{OS: "linux", Architecture: "arm64"}
	multiIdx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add:        imageWithLayers(t, "linux", "amd64", sharedBase, layerFromContent(t, "envoy", "amd64")),
			Descriptor: v1.Descriptor{Platform: &amd64},
		},
		mutate.IndexAddendum{
			Add:        imageWithLayers(t, "linux", "arm64", sharedBase, layerFromContent(t, "envoy", "arm64")),
			Descriptor: v1.Descriptor{Platform: &arm64},
		},
	)
	if err := remote.WriteIndex(parseRef(t, srcRepo+"/envoy-dataplane:stage"), multiIdx); err != nil {
		t.Fatal(err)
	}

	names := []string{"ateapi", "atelet", "envoy-dataplane"}
	wantDigests := make(map[string]string, len(names))
	for _, n := range names {
		wantDigests[n] = indexDigest(t, srcRepo+"/"+n+":stage")
	}

	archiveDir := t.TempDir()
	if err := WriteArchive(t.Context(), archiveDir, srcRepo, "stage", names); err != nil {
		t.Fatalf("WriteArchive() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(archiveDir, "blobs", "sha256", sharedDigest.Hex)); err != nil {
		t.Fatalf("shared base layer blob missing from archive: %v", err)
	}

	gotRefs, err := PushArchive(t.Context(), archiveDir, dstRepo, "e2e")
	if err != nil {
		t.Fatalf("PushArchive() error = %v", err)
	}

	wantRefs := []string{
		dstRepo + "/ateapi:e2e@" + wantDigests["ateapi"],
		dstRepo + "/atelet:e2e@" + wantDigests["atelet"],
		dstRepo + "/envoy-dataplane:e2e@" + wantDigests["envoy-dataplane"],
	}
	slices.Sort(wantRefs)
	if !slices.Equal(gotRefs, wantRefs) {
		t.Errorf("PushArchive() = %v, want %v", gotRefs, wantRefs)
	}

	for _, n := range names {
		got := indexDigest(t, dstRepo+"/"+n+":e2e")
		if got != wantDigests[n] {
			t.Errorf("%s digest after PushArchive = %s, want %s", n, got, wantDigests[n])
		}
	}

	incomplete, err := CheckPublished(t.Context(), []string{dstRepo + "/envoy-dataplane:e2e"}, []string{"linux/amd64", "linux/arm64"})
	if err != nil || len(incomplete) != 0 {
		t.Errorf("CheckPublished(envoy-dataplane) = %v, %v; want complete multi-platform index", incomplete, err)
	}
}

func TestArchiveValidationErrors(t *testing.T) {
	if err := WriteArchive(t.Context(), t.TempDir(), "127.0.0.1:1", "stage", nil); err == nil {
		t.Error("WriteArchive() with no names = nil, want an error")
	}
	if _, err := PushArchive(t.Context(), t.TempDir(), "", "e2e"); err == nil {
		t.Error("PushArchive() with empty repo = nil, want an error")
	}
	if _, err := PushArchive(t.Context(), t.TempDir(), "example.com/substrate", ""); err == nil {
		t.Error("PushArchive() with empty tag = nil, want an error")
	}
	if _, err := PushArchive(t.Context(), t.TempDir(), "example.com/substrate", "bad,tag"); err == nil {
		t.Error("PushArchive() with invalid tag = nil, want an error")
	}
	if _, err := PushArchive(t.Context(), t.TempDir(), "example.com/substrate", "e2e"); err == nil {
		t.Error("PushArchive() on empty directory = nil, want an error")
	}
}
