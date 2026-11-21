#!/usr/bin/env bash

# Copyright 2026 The Kubernetes Authors.
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

set -euo pipefail

dir=${RUNNER_TEMP:-${TMPDIR:-/tmp}}/csi-e2e-tools
mkdir -p "$dir"
curl -fsSL --retry 3 -o "$dir/kubectl" https://dl.k8s.io/release/v1.37.0/bin/linux/amd64/kubectl
printf '%s  %s\n' 6129359f4e1f3848a5572ccb0b26cf28b8ca08cef38c95a765b2f64a2c961a2f "$dir/kubectl" | sha256sum -c -
chmod 0755 "$dir/kubectl"
printf '%s\n' "$dir" >> "${GITHUB_PATH:?GITHUB_PATH is required}"
