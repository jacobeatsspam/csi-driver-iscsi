#!/bin/bash
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

# Resolve only the digest belonging to the repository that was just pushed.
pushed_image_ref() {
    local payload
    payload=$(docker image inspect --format '{{json .RepoDigests}}' "$1") || return
    python3 - "$1" "$payload" <<'PY'
import json, re, sys
image, payload = sys.argv[1:]
repository = image.rsplit(':', 1)[0]

def canonical(repo):
    parts = repo.split('/')
    if len(parts) == 1 or not ('.' in parts[0] or ':' in parts[0] or parts[0] == 'localhost'):
        parts.insert(0, 'docker.io')
    if parts[0] == 'index.docker.io':
        parts[0] = 'docker.io'
    if parts[0] == 'docker.io' and len(parts) == 2:
        parts.insert(1, 'library')
    return '/'.join(parts)

matches = set()
for entry in json.loads(payload) or []:
    repo, separator, digest = entry.rpartition('@')
    if separator and canonical(repo) == canonical(repository) and re.fullmatch(r'sha256:[a-f0-9]{64}', digest):
        matches.add(digest)
if len(matches) != 1:
    raise SystemExit('Cannot determine a unique pushed image digest for ' + repository)
print(repository + '@' + matches.pop())
PY
}
