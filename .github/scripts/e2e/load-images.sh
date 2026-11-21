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

# Loads the artifact produced by .github/scripts/e2e/build-images.sh into a
# running kind-registry container and installs bin/ate-setup.
#
#   .github/scripts/e2e/load-images.sh <in-dir>
#
# Run after hack/create-kind-cluster.sh has started kind-registry and attached
# it to the kind network. Restarting kind-registry clears its in-memory blob
# descriptor cache while keeping its network attachments and port mappings.
#
# Used by the e2e job in .github/workflows/pr-workflow.yaml.

set -o errexit -o nounset -o pipefail

[[ $# -eq 1 ]] || {
  echo "usage: $0 <in-dir>" >&2
  exit 2
}

in_dir="$1"
archive="${in_dir}/registry.tar"
binary="${in_dir}/ate-setup"

for path in "${archive}" "${binary}"; do
  if [[ ! -f "${path}" ]]; then
    echo "error: missing ${path}" >&2
    exit 1
  fi
done

root="$(git rev-parse --show-toplevel)"
reg_name="${KIND_REGISTRY_NAME:-kind-registry}"
reg_port="${KIND_REGISTRY_PORT:-5001}"

mkdir -p "${root}/bin"
install -m 0755 "${binary}" "${root}/bin/ate-setup"

if [[ "$(docker inspect -f '{{.State.Running}}' "${reg_name}" 2>/dev/null || true)" != "true" ]]; then
  echo "error: registry container ${reg_name} is not running; run hack/create-kind-cluster.sh first" >&2
  exit 1
fi

docker exec -i "${reg_name}" tar -C /var/lib/registry -xof - < "${archive}"
docker restart "${reg_name}" >/dev/null

ready=false
for _ in $(seq 1 100); do
  if curl -fsS "http://127.0.0.1:${reg_port}/v2/" >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 0.1
done
if [[ "${ready}" != true ]]; then
  echo "error: registry ${reg_name} did not become ready on 127.0.0.1:${reg_port} after restart" >&2
  docker logs "${reg_name}" >&2 || true
  exit 1
fi
