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

# Local build of kubectl-ate for speed if not present
KUBECTL_ATE="./demos/sandbox/bin/kubectl-ate-local"
if [[ ! -f "${KUBECTL_ATE}" ]]; then
  echo "Building local kubectl-ate for dashboard..."
  go build -o "${KUBECTL_ATE}" ./cmd/kubectl-ate
fi

ATESPACE="sandbox-multiplex"
NAMESPACE="ate-demo-sandbox"

# Setup cache files
PODS_CACHE="./demos/sandbox/bin/pods.json"
WORKERS_CACHE="./demos/sandbox/bin/workers.json"
ACTORS_CACHE="./demos/sandbox/bin/actors.json"

# Remove stale caches on exit and kill background daemon jobs
trap 'rm -f "${PODS_CACHE}" "${WORKERS_CACHE}" "${ACTORS_CACHE}" 2>/dev/null; kill $(jobs -p) 2>/dev/null || true' EXIT

query_daemon() {
  while true; do
    kubectl get pods -n "${NAMESPACE}" -o json 2>/dev/null > "${PODS_CACHE}.tmp" && mv "${PODS_CACHE}.tmp" "${PODS_CACHE}" &
    "${KUBECTL_ATE}" get workers -o json 2>/dev/null > "${WORKERS_CACHE}.tmp" && mv "${WORKERS_CACHE}.tmp" "${WORKERS_CACHE}" &
    "${KUBECTL_ATE}" get actors -a "${ATESPACE}" -o json 2>/dev/null > "${ACTORS_CACHE}.tmp" && mv "${ACTORS_CACHE}.tmp" "${ACTORS_CACHE}" &
    wait
    sleep 0.5
  done
}

# Run query daemon in background
query_daemon &

# Wait for initial cache data to load
clear
echo "Loading dashboard data..."
while [[ ! -f "${PODS_CACHE}" || ! -f "${WORKERS_CACHE}" || ! -f "${ACTORS_CACHE}" ]]; do
  sleep 0.2
done

pad_left() {
  local text="${1}"
  local target_len="${2}"
  local clean_text
  clean_text=$(echo -e "${text}" | sed $'s/\x1b\\[[0-9;]*[a-zA-Z]//g')
  local len=${#clean_text}
  local pad_len=$((target_len - len))
  local padding=""
  if [[ "${pad_len}" -gt 0 ]]; then
    padding=$(printf "%${pad_len}s")
  fi
  echo -e "${text}${padding}"
}

# Color formatting helpers
GREEN="\033[0;32m"
YELLOW="\033[0;33m"
RED="\033[0;31m"
CYAN="\033[0;36m"
BOLD="\033[1m"
NC="\033[0m"
GRAY="\033[0;90m"
WHITE="\033[1;37m"

color_restarts() {
  local restarts="${1}"
  if [[ "${restarts}" -eq 0 ]]; then
    echo -e "${GREEN}0${NC}"
  else
    echo -e "${RED}${BOLD}${restarts}${NC}"
  fi
}

color_density() {
  local density="${1}"
  if [[ "${density}" == "0.0X" ]]; then
    echo -e "${GRAY}${density}${NC}"
  else
    echo -e "${GREEN}${BOLD}${density}${NC}"
  fi
}

color_latency() {
  local val="${1}"
  echo -e "${GREEN}${val} ms${NC}"
}

draw_bar() {
  local active="${1}"
  local total="${2}"
  local color="${3}"

  if [[ "${total}" -eq 0 ]]; then
    echo -e "${GRAY}░░░░░░░░░░ 0%${NC}"
    return
  fi

  local pct=$(( (active * 100) / total ))
  local bar_len=10
  local num_chars=$(( (active * bar_len) / total ))
  local num_dots=$(( bar_len - num_chars ))

  local bar=""
  if [[ "${num_chars}" -gt 0 ]]; then
    for ((i=0; i<num_chars; i++)); do bar+="█"; done
  fi
  local dots=""
  if [[ "${num_dots}" -gt 0 ]]; then
    for ((i=0; i<num_dots; i++)); do dots+="░"; done
  fi

  echo -e "${color}${bar}${GRAY}${dots}${NC} ${WHITE}${pct}%${NC}"
}

# Loop to provide real-time updates
while true; do
  # 1. Query k8s pods for worker pool
  pods_json=$(cat "${PODS_CACHE}" 2>/dev/null || echo "{}")
  total_pods=$(echo "${pods_json}" | jq -r '.items | length')
  total_restarts=$(echo "${pods_json}" | jq -r '[.items[].status.containerStatuses[]?.restartCount] | add')
  if [[ "${total_restarts}" == "null" || -z "${total_restarts}" ]]; then total_restarts=0; fi

  # 2. Query workers via local kubectl-ate
  workers_json=$(cat "${WORKERS_CACHE}" 2>/dev/null || echo "{}")
  pool_workers=$(echo "${workers_json}" | jq -r --arg ns "${NAMESPACE}" '.workers[]? | select(.workerNamespace == $ns)')
  total_workers=$(echo "${pool_workers}" | jq -s 'length')
  assigned_workers=$(echo "${pool_workers}" | jq -s '[.[] | select(.assignment != null)] | length')
  free_workers=$((total_workers - assigned_workers))

  # 3. Query actors via local kubectl-ate
  actors_json=$(cat "${ACTORS_CACHE}" 2>/dev/null || echo "{}")
  total_actors=$(echo "${actors_json}" | jq -r '.actors | length')
  resumed_actors=$(echo "${actors_json}" | jq -r '[.actors[]? | select(.status == "STATUS_RUNNING" or .status == "STATUS_RESUMING")] | length')
  suspended_actors=$(echo "${actors_json}" | jq -r '[.actors[]? | select(.status == "STATUS_SUSPENDED" or .status == "STATUS_SUSPENDING")] | length')

  # Calculate active density multiplier (total actors / assigned workers)
  if [[ "${assigned_workers}" -gt 0 ]]; then
    density=$(( (total_actors * 10) / assigned_workers ))
    density_str="${density%?}.${density: -1}X"
  else
    density_str="0.0X"
  fi

  # 4. Read performance stats from stats.json (written by Go client)
  stats_file="./demos/sandbox/bin/stats.json"
  if [[ -f "${stats_file}" ]]; then
    create_lat=$(jq -r '((.create_latency_p90_ms // 0) * 10 | round / 10)' "${stats_file}" 2>/dev/null || echo "0.0")
    create_tp=$(jq -r '((.create_throughput // 0) * 10 | round / 10)' "${stats_file}" 2>/dev/null || echo "0.0")
    resume_lat=$(jq -r '((.resume_latency_p90_ms // 0) * 10 | round / 10)' "${stats_file}" 2>/dev/null || echo "0.0")
    resume_tp=$(jq -r '((.resume_throughput // 0) * 10 | round / 10)' "${stats_file}" 2>/dev/null || echo "0.0")
    suspend_lat=$(jq -r '((.suspend_latency_p90_ms // 0) * 10 | round / 10)' "${stats_file}" 2>/dev/null || echo "0.0")
    suspend_tp=$(jq -r '((.suspend_throughput // 0) * 10 | round / 10)' "${stats_file}" 2>/dev/null || echo "0.0")
  else
    create_lat="0.0"
    create_tp="0.0"
    resume_lat="0.0"
    resume_tp="0.0"
    suspend_lat="0.0"
    suspend_tp="0.0"
  fi

  # Build side-by-side strings
  left_0="  ${CYAN}${BOLD}SANDBOXED ACTORS${NC}"
  left_1="   Total Actors:        ${WHITE}${total_actors}${NC}"
  left_2="   Resumed (Running):   $(draw_bar "${resumed_actors}" "${total_actors}" "${GREEN}")"
  left_3=""
  left_4="  ${CYAN}${BOLD}DENSITY${NC}"
  left_5="   Active Density:      $(color_density "${density_str}")"
  left_6=""

  right_0="  ${CYAN}${BOLD}SUBSTRATE WORKERS${NC}"
  right_1="   Total Workers:       ${WHITE}${total_workers}${NC}"
  right_2="   Assigned Workers:    $(draw_bar "${assigned_workers}" "${total_workers}" "${GREEN}")"
  right_3=""
  right_4="  ${CYAN}${BOLD}K8S INFRASTRUCTURE${NC}"
  right_5="   Total Pods:          ${WHITE}${total_pods}${NC}"
  right_6="   Pod Restarts:        $(color_restarts "${total_restarts}")"

  c_lat_val=$(color_latency "${create_lat}" 200 500)
  r_lat_val=$(color_latency "${resume_lat}" 500 2000)
  s_lat_val=$(color_latency "${suspend_lat}" 500 2000)

  col1_0="  ${BOLD}${WHITE}CREATE (P90)${NC}"
  col1_1="   Throughput: ${GREEN}${create_tp}${NC} /s"
  col1_2="   Latency:    ${c_lat_val}"

  col2_0="  ${BOLD}${WHITE}RESUME (P90)${NC}"
  col2_1="   Throughput: ${GREEN}${resume_tp}${NC} /s"
  col2_2="   Latency:    ${r_lat_val}"

  col3_0="  ${BOLD}${WHITE}SUSPEND (P90)${NC}"
  col3_1="   Throughput: ${GREEN}${suspend_tp}${NC} /s"
  col3_2="   Latency:    ${s_lat_val}"

  # Move cursor to home (flicker-free rendering)
  printf "\033[H"
  echo -e " ┌────────────────────────────────────────────────────────────────────────────────────────┐"
  echo -e " │                        ${WHITE}${BOLD}AGENT SUBSTRATE SANDBOX MULTIPLEX DEMO${NC}                          │"
  echo -e " │                        Status: ${GREEN}Active${NC}  •  Updated: $(date +'%H:%M:%S')                            │"
  echo -e " └────────────────────────────────────────────────────────────────────────────────────────┘"
  echo -e ""
  echo -e "  $(pad_left "${left_0}" 43) │ $(pad_left "${right_0}" 43)"
  echo -e "  $(pad_left "${left_1}" 43) │ $(pad_left "${right_1}" 43)"
  echo -e "  $(pad_left "${left_2}" 43) │ $(pad_left "${right_2}" 43)"
  echo -e "  $(pad_left "${left_3}" 43) │ $(pad_left "${right_3}" 43)"
  echo -e "  $(pad_left "${left_4}" 43) │ $(pad_left "${right_4}" 43)"
  echo -e "  $(pad_left "${left_5}" 43) │ $(pad_left "${right_5}" 43)"
  echo -e "  $(pad_left "${left_6}" 43) │ $(pad_left "${right_6}" 43)"
  echo -e ""
  echo -e "  ${CYAN}${BOLD}PERFORMANCE TELEMETRY${NC}"
  echo -e "  ┌───────────────────────────┬───────────────────────────┬───────────────────────────┐"
  echo -e "  │$(pad_left "${col1_0}" 27)│$(pad_left "${col2_0}" 27)│$(pad_left "${col3_0}" 27)│"
  echo -e "  │$(pad_left "${col1_1}" 27)│$(pad_left "${col2_1}" 27)│$(pad_left "${col3_1}" 27)│"
  echo -e "  │$(pad_left "${col1_2}" 27)│$(pad_left "${col2_2}" 27)│$(pad_left "${col3_2}" 27)│"
  echo -e "  └───────────────────────────┴───────────────────────────┴───────────────────────────┘"

  sleep 1
done
