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

# Unpacks bin/ate-setup from BUNDLE_DIR, pushes the saved component images into
# the Kind registry under tag e2e, and deletes BUNDLE_DIR.
#
# Used by .github/workflows/pr-workflow.yaml.

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

bundle_dir="${1:?usage: load-e2e-bundle.sh BUNDLE_DIR}"
mkdir -p bin

zstd -d -q -f "${bundle_dir}/ate-setup.zst" -o bin/ate-setup
chmod +x bin/ate-setup
VERSION=e2e bin/ate-setup --kind publish push-images "${bundle_dir}/images"
rm -rf "${bundle_dir}"
