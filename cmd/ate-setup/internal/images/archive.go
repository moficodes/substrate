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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

const (
	// refNameAnnotation is the OCI annotation recording an archived image's
	// repository leaf name in index.json.
	refNameAnnotation = "org.opencontainers.image.ref.name"

	ociLayoutFile     = "oci-layout"
	ociIndexFile      = "index.json"
	ociLayoutContents = `{"imageLayoutVersion":"1.0.0"}` + "\n"
)

// WriteArchive fetches each image in names from repo under tag and writes a
// deduplicated OCI image layout directory to dir.
func WriteArchive(ctx context.Context, dir, repo, tag string, names []string) error {
	if len(names) == 0 {
		return errors.New("no images requested for archive")
	}
	repo = strings.TrimSuffix(repo, "/")
	blobsDir := filepath.Join(dir, "blobs", "sha256")
	for _, stale := range []string{
		filepath.Join(dir, ociIndexFile),
		filepath.Join(dir, ociLayoutFile),
		filepath.Join(dir, "blobs"),
	} {
		if err := os.RemoveAll(stale); err != nil {
			return fmt.Errorf("while clearing %s: %w", stale, err)
		}
	}
	if err := os.MkdirAll(blobsDir, 0o755); err != nil {
		return fmt.Errorf("while creating %s: %w", blobsDir, err)
	}

	manifests := make([]v1.Descriptor, 0, len(names))
	for _, imgName := range names {
		ref := repo + "/" + imgName + ":" + tag
		parsed, err := name.ParseReference(ref)
		if err != nil {
			return fmt.Errorf("%s is not a valid image reference: %w", ref, err)
		}
		desc, err := remote.Get(parsed,
			remote.WithContext(ctx),
			remote.WithAuthFromKeychain(keychain),
		)
		if err != nil {
			return fmt.Errorf("while fetching %s: %w", ref, err)
		}
		saved, err := saveRemoteDescriptor(blobsDir, desc)
		if err != nil {
			return fmt.Errorf("while archiving %s: %w", ref, err)
		}
		saved.Annotations = map[string]string{refNameAnnotation: imgName}
		manifests = append(manifests, saved)
	}

	index := v1.IndexManifest{
		SchemaVersion: 2,
		MediaType:     types.OCIImageIndex,
		Manifests:     manifests,
	}
	indexBytes, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return fmt.Errorf("while encoding %s: %w", ociIndexFile, err)
	}
	indexBytes = append(indexBytes, '\n')
	if err := os.WriteFile(filepath.Join(dir, ociLayoutFile), []byte(ociLayoutContents), 0o644); err != nil {
		return fmt.Errorf("while writing %s: %w", ociLayoutFile, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ociIndexFile), indexBytes, 0o644); err != nil {
		return fmt.Errorf("while writing %s: %w", ociIndexFile, err)
	}
	return nil
}

func saveRemoteDescriptor(blobsDir string, desc *remote.Descriptor) (v1.Descriptor, error) {
	if desc.MediaType.IsIndex() {
		idx, err := desc.ImageIndex()
		if err != nil {
			return v1.Descriptor{}, err
		}
		return saveImageIndex(blobsDir, idx)
	}
	img, err := desc.Image()
	if err != nil {
		return v1.Descriptor{}, err
	}
	return saveImage(blobsDir, img)
}

func saveImageIndex(blobsDir string, idx v1.ImageIndex) (v1.Descriptor, error) {
	im, err := idx.IndexManifest()
	if err != nil {
		return v1.Descriptor{}, err
	}
	for _, child := range im.Manifests {
		if child.MediaType.IsIndex() {
			nested, err := idx.ImageIndex(child.Digest)
			if err != nil {
				return v1.Descriptor{}, err
			}
			if _, err := saveImageIndex(blobsDir, nested); err != nil {
				return v1.Descriptor{}, err
			}
			continue
		}
		img, err := idx.Image(child.Digest)
		if err != nil {
			return v1.Descriptor{}, err
		}
		if _, err := saveImage(blobsDir, img); err != nil {
			return v1.Descriptor{}, err
		}
	}
	raw, err := idx.RawManifest()
	if err != nil {
		return v1.Descriptor{}, err
	}
	mt, err := idx.MediaType()
	if err != nil {
		return v1.Descriptor{}, err
	}
	h, size, err := writeBytesBlob(blobsDir, raw)
	if err != nil {
		return v1.Descriptor{}, err
	}
	return v1.Descriptor{MediaType: mt, Size: size, Digest: h}, nil
}

func saveImage(blobsDir string, img v1.Image) (v1.Descriptor, error) {
	rawCfg, err := img.RawConfigFile()
	if err != nil {
		return v1.Descriptor{}, err
	}
	if _, _, err := writeBytesBlob(blobsDir, rawCfg); err != nil {
		return v1.Descriptor{}, err
	}
	layers, err := img.Layers()
	if err != nil {
		return v1.Descriptor{}, err
	}
	for _, layer := range layers {
		d, err := layer.Digest()
		if err != nil {
			return v1.Descriptor{}, err
		}
		dst, err := blobPath(blobsDir, d)
		if err != nil {
			return v1.Descriptor{}, err
		}
		if _, err := os.Stat(dst); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return v1.Descriptor{}, err
		}
		rc, err := layer.Compressed()
		if err != nil {
			return v1.Descriptor{}, err
		}
		err = writeReaderBlob(dst, rc)
		closeErr := rc.Close()
		if err != nil {
			return v1.Descriptor{}, err
		}
		if closeErr != nil {
			return v1.Descriptor{}, closeErr
		}
	}
	rawManifest, err := img.RawManifest()
	if err != nil {
		return v1.Descriptor{}, err
	}
	mt, err := img.MediaType()
	if err != nil {
		return v1.Descriptor{}, err
	}
	h, size, err := writeBytesBlob(blobsDir, rawManifest)
	if err != nil {
		return v1.Descriptor{}, err
	}
	return v1.Descriptor{MediaType: mt, Size: size, Digest: h}, nil
}

func blobPath(blobsDir string, h v1.Hash) (string, error) {
	if h.Algorithm != "sha256" || len(h.Hex) != 64 {
		return "", fmt.Errorf("unsupported digest %q", h)
	}
	for _, r := range h.Hex {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", fmt.Errorf("invalid sha256 hex %q", h.Hex)
		}
	}
	return filepath.Join(blobsDir, h.Hex), nil
}

func writeBytesBlob(blobsDir string, raw []byte) (v1.Hash, int64, error) {
	h, size, err := v1.SHA256(bytes.NewReader(raw))
	if err != nil {
		return v1.Hash{}, 0, err
	}
	dst, err := blobPath(blobsDir, h)
	if err != nil {
		return v1.Hash{}, 0, err
	}
	if _, err := os.Stat(dst); err == nil {
		return h, size, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return v1.Hash{}, 0, err
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		return v1.Hash{}, 0, err
	}
	return h, size, nil
}

func writeReaderBlob(dst string, r io.Reader) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".blob-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.Copy(tmp, r); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// PushArchive pushes every image recorded in the OCI layout at dir to repo
// under tag, returning the pushed digest-pinned references in sorted order.
func PushArchive(ctx context.Context, dir, repo, tag string) ([]string, error) {
	repo = strings.TrimSuffix(repo, "/")
	if repo == "" {
		return nil, errors.New("image repository must not be empty")
	}
	if tag == "" {
		return nil, errors.New("image tag must not be empty")
	}
	if _, err := name.NewTag(repo+"/image:"+tag, name.StrictValidation); err != nil {
		return nil, fmt.Errorf("%s with tag %s is not a valid image reference: %w", repo, tag, err)
	}

	rawIndex, err := os.ReadFile(filepath.Join(dir, ociIndexFile))
	if err != nil {
		return nil, fmt.Errorf("while reading %s in %s: %w", ociIndexFile, dir, err)
	}
	var index v1.IndexManifest
	if err := json.Unmarshal(rawIndex, &index); err != nil {
		return nil, fmt.Errorf("while parsing %s in %s: %w", ociIndexFile, dir, err)
	}
	if len(index.Manifests) == 0 {
		return nil, fmt.Errorf("image archive %s lists no images", dir)
	}

	blobsDir := filepath.Join(dir, "blobs", "sha256")
	todo := make(map[name.Reference]remote.Taggable, len(index.Manifests))
	refs := make([]string, 0, len(index.Manifests))
	for _, desc := range index.Manifests {
		imgName := desc.Annotations[refNameAnnotation]
		if imgName == "" {
			return nil, fmt.Errorf("manifest %s in %s has no %s annotation", desc.Digest, dir, refNameAnnotation)
		}
		target, err := name.NewTag(repo + "/" + imgName + ":" + tag)
		if err != nil {
			return nil, fmt.Errorf("invalid target image reference for %s: %w", imgName, err)
		}
		taggable, err := loadArchivedTaggable(blobsDir, desc)
		if err != nil {
			return nil, fmt.Errorf("while loading %s from %s: %w", imgName, dir, err)
		}
		todo[target] = taggable
		refs = append(refs, target.Name()+"@"+desc.Digest.String())
	}

	if err := remote.MultiWrite(todo,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(keychain),
	); err != nil {
		return nil, fmt.Errorf("while pushing archived images to %s: %w", repo, err)
	}
	slices.Sort(refs)
	return refs, nil
}

func loadArchivedTaggable(blobsDir string, desc v1.Descriptor) (remote.Taggable, error) {
	if desc.MediaType.IsIndex() {
		return loadArchivedIndex(blobsDir, desc)
	}
	return loadArchivedImage(blobsDir, desc)
}

func loadArchivedIndex(blobsDir string, desc v1.Descriptor) (v1.ImageIndex, error) {
	p, err := blobPath(blobsDir, desc.Digest)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	im, err := v1.ParseIndexManifest(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return &archivedIndex{
		blobsDir:    blobsDir,
		desc:        desc,
		rawManifest: raw,
		manifest:    im,
	}, nil
}

type archivedIndex struct {
	blobsDir    string
	desc        v1.Descriptor
	rawManifest []byte
	manifest    *v1.IndexManifest
}

func (a *archivedIndex) MediaType() (types.MediaType, error)       { return a.desc.MediaType, nil }
func (a *archivedIndex) Digest() (v1.Hash, error)                  { return a.desc.Digest, nil }
func (a *archivedIndex) Size() (int64, error)                      { return int64(len(a.rawManifest)), nil }
func (a *archivedIndex) IndexManifest() (*v1.IndexManifest, error) { return a.manifest, nil }
func (a *archivedIndex) RawManifest() ([]byte, error)              { return a.rawManifest, nil }

func (a *archivedIndex) Image(h v1.Hash) (v1.Image, error) {
	for _, m := range a.manifest.Manifests {
		if m.Digest == h && !m.MediaType.IsIndex() {
			return loadArchivedImage(a.blobsDir, m)
		}
	}
	return nil, fmt.Errorf("image %s missing from index %s", h, a.desc.Digest)
}

func (a *archivedIndex) ImageIndex(h v1.Hash) (v1.ImageIndex, error) {
	for _, m := range a.manifest.Manifests {
		if m.Digest == h && m.MediaType.IsIndex() {
			return loadArchivedIndex(a.blobsDir, m)
		}
	}
	return nil, fmt.Errorf("nested index %s missing from index %s", h, a.desc.Digest)
}

func loadArchivedImage(blobsDir string, desc v1.Descriptor) (v1.Image, error) {
	p, err := blobPath(blobsDir, desc.Digest)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	m, err := v1.ParseManifest(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if _, err := blobPath(blobsDir, m.Config.Digest); err != nil {
		return nil, err
	}
	return partial.CompressedToImage(&archivedImage{
		blobsDir:    blobsDir,
		mediaType:   desc.MediaType,
		rawManifest: raw,
		manifest:    m,
	})
}

type archivedImage struct {
	blobsDir    string
	mediaType   types.MediaType
	rawManifest []byte
	manifest    *v1.Manifest
}

func (a *archivedImage) MediaType() (types.MediaType, error) { return a.mediaType, nil }
func (a *archivedImage) RawManifest() ([]byte, error)        { return a.rawManifest, nil }

func (a *archivedImage) RawConfigFile() ([]byte, error) {
	p, err := blobPath(a.blobsDir, a.manifest.Config.Digest)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

func (a *archivedImage) LayerByDigest(h v1.Hash) (partial.CompressedLayer, error) {
	for _, l := range a.manifest.Layers {
		if l.Digest == h {
			p, err := blobPath(a.blobsDir, l.Digest)
			if err != nil {
				return nil, err
			}
			return &archivedLayer{path: p, desc: l}, nil
		}
	}
	return nil, fmt.Errorf("layer %s missing from manifest", h)
}

type archivedLayer struct {
	path string
	desc v1.Descriptor
}

func (l *archivedLayer) Digest() (v1.Hash, error)            { return l.desc.Digest, nil }
func (l *archivedLayer) Compressed() (io.ReadCloser, error)  { return os.Open(l.path) }
func (l *archivedLayer) Size() (int64, error)                { return l.desc.Size, nil }
func (l *archivedLayer) MediaType() (types.MediaType, error) { return l.desc.MediaType, nil }
