#!/usr/bin/env python3
"""Network-free contract checks for the opt-in WAN drill."""
import json
from pathlib import Path
import subprocess
import sys
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import check_wan_multi_server as wan


class WANMultiServerLocalTests(unittest.TestCase):
    def test_two_distinct_narrow_hosts_required(self):
        wan.validate_hosts(["alice@one.example", "bob@two.example"])
        for invalid in ([], ["alice@one.example"], ["alice@one.example"] * 2,
                        ["-oProxyCommand=bad", "bob@two.example"],
                        ["alice@one.example;bad", "bob@two.example"]):
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                wan.validate_hosts(invalid)

    def test_remote_temp_path_is_narrow(self):
        self.assertTrue(wan.REMOTE_DIR.fullmatch("/tmp/continuum-wan.Abc123"))
        for invalid in ("/tmp/continuum-wan.Abc123/../other", "/home/user/test",
                        "/tmp/continuum-wan.Abc123;bad"):
            self.assertIsNone(wan.REMOTE_DIR.fullmatch(invalid))

    def test_platform_allowlist(self):
        self.assertEqual(wan.PLATFORMS[("Darwin", "arm64")], ("darwin", "arm64"))
        self.assertEqual(wan.PLATFORMS[("Linux", "x86_64")], ("linux", "amd64"))
        self.assertNotIn(("Windows", "amd64"), wan.PLATFORMS)

    def test_wait_assertions_reject_partial_or_unordered_all(self):
        valid = {"state": "satisfied", "mode": "all", "completion_order": 7,
                 "sources": [{"name": "alpha", "matched": True, "completion_order": 2},
                             {"name": "bravo", "matched": True, "completion_order": 4},
                             {"name": "local", "matched": True, "completion_order": 6}]}
        wan.assert_wait(valid, "all", ("alpha", "bravo", "local"))
        for change in ({"state": "pending"}, {"completion_order": 0},
                       {"sources": valid["sources"][:2]},
                       {"sources": [valid["sources"][0], valid["sources"][1],
                                    {"name": "local", "matched": True, "completion_order": 4}]}):
            with self.subTest(change=change), self.assertRaises(RuntimeError):
                wan.assert_wait(dict(valid, **change), "all", ("alpha", "bravo", "local"))

    def test_any_winner_is_checked(self):
        valid = {"state": "satisfied", "mode": "any", "completion_order": 3, "winner": "alpha",
                 "sources": [{"name": name} for name in ("alpha", "bravo", "local")]}
        wan.assert_wait(valid, "any", ("alpha", "bravo", "local"), "alpha")
        with self.assertRaises(RuntimeError):
            wan.assert_wait(dict(valid, winner="bravo"), "any", ("alpha", "bravo", "local"), "alpha")

    def test_plan_has_no_remote_side_effects(self):
        script = Path(wan.__file__)
        proc = subprocess.run([sys.executable, str(script), "--plan"], capture_output=True,
                              text=True, check=True, timeout=5)
        plan = json.loads(proc.stdout)
        self.assertEqual(plan["class"], "planned")
        self.assertTrue(plan["remote_run_required"])
        self.assertEqual(plan["hosts"], 3)
        self.assertGreaterEqual(len(plan["cases"]), 5)


if __name__ == "__main__":
    unittest.main()
