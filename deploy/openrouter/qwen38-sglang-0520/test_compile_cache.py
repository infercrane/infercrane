import argparse
import hashlib
import importlib.util
import pathlib
import tempfile
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("compile_cache.py")
SPEC = importlib.util.spec_from_file_location("compile_cache", MODULE_PATH)
compile_cache = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(compile_cache)


def identity(**overrides):
    values = {
        "runtime_image": "runtime@sha256:abc",
        "model_revision": "model-revision",
        "gpu_arch": "sm90",
        "cuda_version": "13.0",
    }
    values.update(overrides)
    return values


class CompiledCacheTest(unittest.TestCase):
    def test_publish_and_restore_exact_release(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            source = root / "source"
            (source / "triton").mkdir(parents=True)
            (source / "triton" / "kernel.cubin").write_bytes(b"compiled")
            compile_cache.publish(
                argparse.Namespace(source=str(source), releases=str(root / "releases"), release="r1", **identity())
            )
            release = root / "releases" / "r1"
            digest = hashlib.sha256((release / "manifest.json").read_bytes()).hexdigest()
            destination = root / "local-cache"
            compile_cache.restore(
                argparse.Namespace(release_dir=str(release), manifest_sha256=digest, destination=str(destination), **identity())
            )
            self.assertEqual((destination / "triton" / "kernel.cubin").read_bytes(), b"compiled")
            self.assertTrue((destination / compile_cache.SENTINEL).is_file())

    def test_restore_replaces_empty_destination(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            source = root / "source"
            source.mkdir()
            (source / "kernel.bin").write_bytes(b"compiled")
            compile_cache.publish(
                argparse.Namespace(source=str(source), releases=str(root / "releases"), release="r1", **identity())
            )
            release = root / "releases" / "r1"
            digest = hashlib.sha256((release / "manifest.json").read_bytes()).hexdigest()
            destination = root / "local-cache"
            destination.mkdir()
            compile_cache.restore(
                argparse.Namespace(release_dir=str(release), manifest_sha256=digest, destination=str(destination), **identity())
            )
            self.assertEqual((destination / "kernel.bin").read_bytes(), b"compiled")

    def test_restore_rejects_tampering_and_identity_drift(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            source = root / "source"
            source.mkdir()
            (source / "kernel.bin").write_bytes(b"original")
            compile_cache.publish(
                argparse.Namespace(source=str(source), releases=str(root / "releases"), release="r1", **identity())
            )
            release = root / "releases" / "r1"
            digest = hashlib.sha256((release / "manifest.json").read_bytes()).hexdigest()
            (release / "cache" / "kernel.bin").write_bytes(b"tampered")
            with self.assertRaisesRegex(ValueError, "content"):
                compile_cache.restore(
                    argparse.Namespace(release_dir=str(release), manifest_sha256=digest, destination=str(root / "target"), **identity())
                )
            (release / "cache" / "kernel.bin").write_bytes(b"original")
            with self.assertRaisesRegex(ValueError, "compatibility"):
                compile_cache.restore(
                    argparse.Namespace(release_dir=str(release), manifest_sha256=digest, destination=str(root / "target"), **identity(gpu_arch="sm100"))
                )

    def test_publish_rejects_symlink(self):
        with tempfile.TemporaryDirectory() as raw:
            root = pathlib.Path(raw)
            source = root / "source"
            source.mkdir()
            (source / "outside").symlink_to("/etc/passwd")
            with self.assertRaisesRegex(ValueError, "symlinks"):
                compile_cache.publish(
                    argparse.Namespace(source=str(source), releases=str(root / "releases"), release="r1", **identity())
                )


if __name__ == "__main__":
    unittest.main()
