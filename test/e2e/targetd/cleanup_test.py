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

import unittest

from cleanup_target import TARGET, cleanup


class Target:
    def __init__(self, wwn):
        self.wwn = wwn
        self.deleted = False

    def delete(self):
        self.deleted = True


class CleanupTest(unittest.TestCase):
    def test_present_and_absent(self):
        owned = Target(TARGET)
        other = Target("iqn.other:target")
        cleanup([owned, other])
        self.assertTrue(owned.deleted)
        self.assertFalse(other.deleted)
        cleanup([other])
        self.assertFalse(other.deleted)

    def test_ambiguous(self):
        with self.assertRaises(RuntimeError):
            cleanup([Target(TARGET), Target(TARGET)])


if __name__ == "__main__":
    unittest.main()
