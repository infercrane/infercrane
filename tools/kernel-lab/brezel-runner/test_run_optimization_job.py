#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import importlib.machinery
import importlib.util
import json
import tempfile
import unittest
from pathlib import Path

RUNNER = Path(__file__).with_name("run-optimization-job")
loader = importlib.machinery.SourceFileLoader("run_optimization_job", str(RUNNER))
spec = importlib.util.spec_from_loader("run_optimization_job", loader)
runner = importlib.util.module_from_spec(spec)
loader.exec_module(runner)


class RunnerTest(unittest.TestCase):
    def execute(self, source: bytes, backend: str = "triton", digest: str | None = None) -> tuple[dict, Path, bytes]:
        root = Path(tempfile.mkdtemp())
        (root / "input" / "artifacts").mkdir(parents=True)
        (root / "input" / "artifacts" / "source-01").write_bytes(source)
        job = {
            "schema_version": runner.JOB_SCHEMA,
            "id": "lab_1",
            "candidate": {"backend": backend, "license": "Apache-2.0"},
            "inputs": [{"name": "source-01", "sha256": digest or hashlib.sha256(source).hexdigest()}],
        }
        manifest = root / "input" / "job.json"
        manifest_bytes = json.dumps(job).encode()
        manifest.write_bytes(manifest_bytes)
        result_path = root / "output" / "result.json"
        self.assertEqual(runner.run(manifest, result_path), 0)
        return json.loads(result_path.read_text()), root, manifest_bytes

    def test_valid_source_is_published_with_bound_digests(self) -> None:
        source = b"def fused():\n    return 1\n"
        result, root, manifest_bytes = self.execute(source)
        self.assertEqual(result["status"], "passed")
        self.assertEqual(result["input_digest"], hashlib.sha256(manifest_bytes).hexdigest())
        artifact = result["artifacts"][0]
        self.assertEqual(artifact["sha256"], hashlib.sha256(source).hexdigest())
        self.assertEqual(Path(artifact["path"]).read_bytes(), source)
        self.assertTrue(Path(artifact["path"]).is_relative_to(root / "output" / "artifacts"))

    def test_source_is_compiled_but_never_executed(self) -> None:
        marker = Path(tempfile.mkdtemp()) / "executed"
        source = f"open({str(marker)!r}, 'w').close()\n".encode()
        result, _, _ = self.execute(source)
        self.assertEqual(result["status"], "passed")
        self.assertFalse(marker.exists())

    def test_tampered_or_invalid_source_fails_closed(self) -> None:
        for source, digest, backend in (
            (b"x = 1\n", "0" * 64, "triton"),
            (b"def broken(:\n", None, "triton"),
            (b"x = 1\n", None, "cuda"),
            (b"\xff\xfe", None, "triton"),
        ):
            result, _, _ = self.execute(source, backend, digest)
            self.assertEqual(result["status"], "rejected", source)
            self.assertEqual(result["artifacts"], [], source)


if __name__ == "__main__":
    unittest.main()
