import importlib.util
import pathlib
import unittest


MODULE_PATH = pathlib.Path(__file__).with_name("patch_gdn_precision.py")
SPEC = importlib.util.spec_from_file_location("patch_gdn_precision", MODULE_PATH)
patch = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(patch)


class GDNPrecisionPatchTest(unittest.TestCase):
    def test_patch_requires_exactly_one_vulnerable_expression(self):
        self.assertEqual(patch.patch_text("before old after", "old", "new"), "before new after")
        for text in ("missing", "old and old"):
            with self.assertRaisesRegex(ValueError, "exactly once"):
                patch.patch_text(text, "old", "new")

    def test_every_patch_is_pinned_to_source_and_upstream_commit(self):
        for value in patch.PATCHES.values():
            self.assertEqual(len(value["before_sha256"]), 64)
            self.assertEqual(len(value["after_sha256"]), 64)
            self.assertNotEqual(value["before_sha256"], value["after_sha256"])
            self.assertIn("@", value["upstream"])
            self.assertNotEqual(value["old"], value["new"])


if __name__ == "__main__":
    unittest.main()
