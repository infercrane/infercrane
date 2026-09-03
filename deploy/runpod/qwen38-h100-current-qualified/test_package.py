from __future__ import annotations

import hashlib
import json
import pathlib
import unittest


PACKAGE_DIR = pathlib.Path(__file__).resolve().parent


class DeploymentPackageTest(unittest.TestCase):
    def setUp(self) -> None:
        self.manifest = json.loads((PACKAGE_DIR / "manifest.json").read_text())

    def test_manifest_fails_closed_until_runpod_qualification(self) -> None:
        self.assertFalse(self.manifest["final_release"])
        self.assertFalse(self.manifest["production_publication_allowed"])
        self.assertTrue(self.manifest["publication_gate"]["fail_closed"])
        self.assertEqual(self.manifest["qualification"]["runpod"]["state"], "not_started")
        self.assertFalse(self.manifest["source_handoff"]["runpod_qualification_inherited"])

    def test_runpod_contract_is_load_balanced_and_exact(self) -> None:
        runpod = self.manifest["runpod"]
        self.assertEqual(runpod["endpoint_mode"], "load_balancer")
        self.assertEqual(runpod["gpu"]["model"], "NVIDIA H100 80GB HBM3")
        self.assertEqual(runpod["gpu"]["count"], 1)
        self.assertEqual(runpod["qualification_worker_policy"]["workers_min"], 0)
        self.assertEqual(runpod["qualification_worker_policy"]["workers_max"], 1)
        self.assertEqual(runpod["template"]["docker_entrypoint_override"], None)
        self.assertEqual(runpod["template"]["docker_start_command_override"], None)
        self.assertEqual(runpod["template"]["environment"]["PORT_HEALTH"], "30001")
        self.assertEqual(runpod["template"]["environment"]["HEALTH_CHECK_PATH"], "/ping")

    def test_identity_and_launch_vector_are_pinned(self) -> None:
        self.assertEqual(
            self.manifest["model"]["revision"],
            "017b9c7af6b5689d5dd426a76e0bc077eb5ca20a",
        )
        self.assertIn(
            "@sha256:", self.manifest["runtime"]["base_image"]
        )
        self.assertFalse(self.manifest["launch"]["argument_vector_mutable"])
        self.assertFalse(self.manifest["launch"]["entrypoint_override_allowed"])
        self.assertFalse(self.manifest["launch"]["start_command_override_allowed"])

        serve_script = (PACKAGE_DIR / "serve.sh").read_text()
        self.assertNotIn('"$@"', serve_script)
        self.assertIn("if (( $# != 0 )); then", serve_script)
        self.assertIn("--cuda-graph-max-bs 8", serve_script)
        self.assertIn("--disable-piecewise-cuda-graph", serve_script)
        self.assertIn("HF_HUB_OFFLINE=1", serve_script)
        self.assertIn("artifact manifest revision does not match", serve_script)

    def test_source_handoff_manifest_hash_is_preserved(self) -> None:
        source = self.manifest["source_handoff"]
        source_path = (
            PACKAGE_DIR.parents[2].parent
            / source["repository"]
            / source["path"]
            / "manifest.json"
        )
        if not source_path.exists():
            self.skipTest("source handoff is not present in this checkout")
        digest = hashlib.sha256(source_path.read_bytes()).hexdigest()
        self.assertEqual(digest, source["manifest_sha256"])

    def test_package_checksums(self) -> None:
        for line in (PACKAGE_DIR / "SHA256SUMS").read_text().splitlines():
            expected, relative_path = line.split("  ", maxsplit=1)
            artifact = PACKAGE_DIR / relative_path
            self.assertTrue(artifact.is_file(), relative_path)
            self.assertEqual(hashlib.sha256(artifact.read_bytes()).hexdigest(), expected)


if __name__ == "__main__":
    unittest.main()
