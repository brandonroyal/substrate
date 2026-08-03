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

# Ensure port-forwards are running
if ! lsof -i :8080 >/dev/null 2>&1; then
  echo "Starting port-forward for substrate API to localhost:8080 in background..."
  nohup kubectl port-forward -n ate-system svc/api 8080:443 >/dev/null 2>&1 &
  sleep 2
fi

if ! lsof -i :8000 >/dev/null 2>&1; then
  echo "Starting port-forward for atenet-router to localhost:8000 in background..."
  nohup kubectl port-forward -n ate-system svc/atenet-router 8000:80 >/dev/null 2>&1 &
  sleep 2
fi

SESSION_NAME="substrate-demo"

# Kill old session if exists
tmux kill-session -t "${SESSION_NAME}" 2>/dev/null || true

echo "Starting tmux session '${SESSION_NAME}'..."

# Create a new session window
tmux new-session -d -s "${SESSION_NAME}" -n "sandbox-multiplex"

# Enable mouse support for resizing/scrolling
tmux set -g mouse on

# Split window horizontally (Left pane gets 60%, Right pane gets 40% of width)
tmux split-window -h -p 40 -t "${SESSION_NAME}:sandbox-multiplex"

# Prepare commands
CMD_CLIENT="go run ./demos/sandbox/client/multiplex/ --ateapi=localhost:8080 --atenet=localhost:8000 --atespace=sandbox-multiplex --actors=1000 --concurrency=200"
CMD_DASHBOARD="./demos/sandbox/summary_dashboard.sh"

# Send commands to panes
tmux send-keys -t "${SESSION_NAME}:sandbox-multiplex.0" "${CMD_CLIENT}" Enter
tmux send-keys -t "${SESSION_NAME}:sandbox-multiplex.1" "${CMD_DASHBOARD}" Enter

# Select the left pane (running the client)
tmux select-pane -t "${SESSION_NAME}:sandbox-multiplex.0"

# Attach to the session
tmux attach-session -t "${SESSION_NAME}"
