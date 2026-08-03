#!/usr/bin/env bash

# Copyright 2026 Google LLC
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

set -o errexit -o nounset -o pipefail

ROOT="$(git rev-parse --show-toplevel)"
cd "${ROOT}"

# Source the environment variables if configured
if [[ -f .ate-dev-env.sh ]]; then
  source .ate-dev-env.sh
fi

# Ensure port-forward is running on 8080 (required for kubectl-ate)
PF_PID=""
cleanup() {
  if [[ -n "${PF_PID}" ]]; then
    echo "Cleaning up background API port-forward..."
    kill "${PF_PID}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

if ! lsof -i :8080 >/dev/null 2>&1; then
  echo "Starting port-forward for substrate API to localhost:8080..."
  kubectl port-forward -n ate-system svc/api 8080:443 &
  PF_PID=$!
  sleep 2
fi

ATESPACE="sandbox-multiplex"
echo "Cleaning up all actors in atespace '${ATESPACE}'..."

# Build kubectl-ate
go install ./cmd/kubectl-ate

# List actors in JSON format
actors_json=$(go run ./cmd/kubectl-ate get actors -A -o json 2>/dev/null || echo "{}")

# Parse and clean up actors
if command -v jq &>/dev/null; then
  # For each actor in the sandbox-multiplex atespace, suspend and delete it
  while IFS=$'\t' read -r actor_id status; do
    if [[ -n "${actor_id}" ]]; then
      if [[ "${status}" != "STATUS_SUSPENDED" && "${status}" != "STATUS_SUSPENDING" ]]; then
        echo "Actor ${actor_id} is in status ${status}, suspending first..."
        go run ./cmd/kubectl-ate suspend actor "${actor_id}" -a "${ATESPACE}" || true
        sleep 0.5
      fi
      echo "Deleting actor ${actor_id}..."
      go run ./cmd/kubectl-ate delete actor "${actor_id}" -a "${ATESPACE}" || true
    fi
  done < <(
    jq -r --arg atespace "${ATESPACE}" \
      '.actors[]? | select(.atespace == $atespace) | "\(.actorId)\t\(.status)"' \
      <<<"${actors_json}"
  )
else
  # Fallback if jq is not installed
  echo "jq not found, parsing standard output..."
  # Skip header line, get actor IDs from first column
  actor_ids=$(go run ./cmd/kubectl-ate get actors -a "${ATESPACE}" | tail -n +2 | awk '{print $4}')
  for actor_id in ${actor_ids}; do
    if [[ -n "${actor_id}" ]]; then
      echo "Suspending actor ${actor_id}..."
      go run ./cmd/kubectl-ate suspend actor "${actor_id}" -a "${ATESPACE}" || true
      sleep 0.5
      echo "Deleting actor ${actor_id}..."
      go run ./cmd/kubectl-ate delete actor "${actor_id}" -a "${ATESPACE}" || true
    fi
  done
fi

echo "Cleanup complete!"
