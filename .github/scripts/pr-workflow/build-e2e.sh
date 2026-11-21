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

# Builds ate-setup and every installable component image into BUNDLE_DIR so E2E
# matrix jobs can load pre-built artifacts instead of recompiling per lane.
#
# Used by .github/workflows/pr-workflow.yaml.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

bundle_dir="${1:?usage: build-e2e.sh BUNDLE_DIR}"
mkdir -p bin "${bundle_dir}"

CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/ate-setup ./cmd/ate-setup
zstd -1 -q -f bin/ate-setup -o "${bundle_dir}/ate-setup.zst"
bin/ate-setup --kind publish save-images "${bundle_dir}/images"
