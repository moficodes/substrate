#!/usr/bin/env bash

# Copyright 2026 The Agent Substrate Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Builds the container images and ate-setup binary that E2E clusters deploy,
# writing <out-dir>/registry.tar and <out-dir>/ate-setup.
#
#   DOCKER_BUILD_FLAGS="..." .github/scripts/e2e/build-images.sh <out-dir>
#
# An ephemeral registry:3 container receives both pushes concurrently:
# envoy-dataplane from docker buildx, and every images.Components package from
# a single ko build invocation. Archiving /var/lib/registry lets each e2e
# matrix lane seed its kind-registry and install with --image-repo/--image-tag.
#
# Used by the build-e2e-images job in .github/workflows/pr-workflow.yaml.

set -o errexit -o nounset -o pipefail

[[ $# -eq 1 ]] || {
  echo "usage: $0 <out-dir>" >&2
  exit 2
}

mkdir -p "$1"
out_dir="$(cd "$1" && pwd)"

root="$(git rev-parse --show-toplevel)"
cd "${root}"

# Keep in sync with images.Components in cmd/ate-setup/internal/images/images.go
# (checked by TestE2EBuildScriptCoversComponents).
COMPONENTS=(
  cmd/ate-idp-server
  cmd/ateapi
  cmd/atecontroller
  cmd/atelet
  cmd/atenet
  cmd/ateworker-gvisor
  cmd/ateworker-microvm
  cmd/benchmarking/isolate/fake-atelet
  cmd/benchmarking/isolate/fake-workersync
  cmd/credential-provider/kubernetes-secrets
  cmd/objectstore-plugin
  cmd/podcertcontroller
  demos/counter
  demos/egress
  demos/multi-template/fspersist
  demos/sandbox
)

# Pinned to the same registry:3 digest as .github/workflows/release.yaml.
reg_image="registry:3@sha256:ddf754342cfc8acc51a56d5d0ab6af06826461864460636d8bd5c546dab2a7b8"
reg_name="${E2E_BUILD_REGISTRY_NAME:-e2e-build-registry}"
reg_port="${E2E_BUILD_REGISTRY_PORT:-5001}"
tag="${E2E_IMAGE_TAG:-e2e}"
platform="${KO_DEFAULTPLATFORMS:-linux/$(go env GOARCH)}"

work_dir="$(mktemp -d)"
cleanup() {
  docker rm -f "${reg_name}" >/dev/null 2>&1 || true
  rm -rf "${work_dir}"
}
trap cleanup EXIT

docker rm -f "${reg_name}" >/dev/null 2>&1 || true
docker run -d \
  -p "127.0.0.1:${reg_port}:5000" \
  --name "${reg_name}" \
  "${reg_image}" >/dev/null

ready=false
for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:${reg_port}/v2/" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.1
done
if [[ "${ready}" != true ]]; then
  echo "error: build registry ${reg_name} did not become ready on 127.0.0.1:${reg_port}" >&2
  docker logs "${reg_name}" >&2 || true
  exit 1
fi

# Match cmd/ate-setup/internal/ko.Ldflags so ko and go build share build-cache
# entries and stamp identical version metadata.
version="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
version_pkg="go.ate.dev/substrate/internal/version"
ko_ldflags=("--ldflags=-X=${version_pkg}.Version=${version}")
go_ldflags=("-X=${version_pkg}.Version=${version}")
if stamp="$(git show -s --format='%H %cI' HEAD 2>/dev/null)" && [[ -n "${stamp}" ]]; then
  read -r commit build_date <<<"${stamp}"
  ko_ldflags+=(
    "--ldflags=-X=${version_pkg}.Commit=${commit}"
    "--ldflags=-X=${version_pkg}.BuildDate=${build_date}"
  )
  go_ldflags+=(
    "-X=${version_pkg}.Commit=${commit}"
    "-X=${version_pkg}.BuildDate=${build_date}"
  )
fi
go_ldflags_str="${go_ldflags[*]}"

ko_bin="$("${root}/hack/run-tool.sh" --print-bin-path ko)"
ko_pkgs=()
for pkg in "${COMPONENTS[@]}"; do
  ko_pkgs+=("./${pkg}")
done

extra_flags=()
if [[ -n "${DOCKER_BUILD_FLAGS:-}" ]]; then
  read -r -a extra_flags <<<"${DOCKER_BUILD_FLAGS//$'\n'/ }"
fi

(
  KO_DOCKER_REPO="localhost:${reg_port}" \
  KO_DEFAULTPLATFORMS="${platform}" \
    "${ko_bin}" build --base-import-paths --tags="${tag}" "${ko_ldflags[@]}" "${ko_pkgs[@]}" \
  && CGO_ENABLED=0 go build -trimpath -ldflags="${go_ldflags_str}" -o "${out_dir}/ate-setup" ./cmd/ate-setup
) &
go_pid=$!

(
  docker buildx build \
    --platform="${platform}" \
    --push \
    ${extra_flags[@]+"${extra_flags[@]}"} \
    -t "localhost:${reg_port}/envoy-dataplane:${tag}" \
    "${root}/cmd/dataplane/envoy" \
    >"${work_dir}/envoy.log" 2>&1
) &
envoy_pid=$!

go_ok=true
wait "${go_pid}" || go_ok=false
envoy_ok=true
wait "${envoy_pid}" || envoy_ok=false

echo "=== envoy-dataplane build ===" >&2
cat "${work_dir}/envoy.log" >&2

if [[ "${go_ok}" != true || "${envoy_ok}" != true ]]; then
  exit 1
fi

accept="application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"
expected_images=("envoy-dataplane")
for pkg in "${COMPONENTS[@]}"; do
  expected_images+=("${pkg##*/}")
done
for img in "${expected_images[@]}"; do
  if ! curl -fsSI -H "Accept: ${accept}" \
      "http://127.0.0.1:${reg_port}/v2/${img}/manifests/${tag}" >/dev/null; then
    echo "error: expected image localhost:${reg_port}/${img}:${tag} is missing from build registry" >&2
    exit 1
  fi
done

docker exec "${reg_name}" tar -C /var/lib/registry -cf - . > "${out_dir}/registry.tar"
