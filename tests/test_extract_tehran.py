"""Checks for the published development extract and provenance permissions."""

import importlib.util
import json
import os
from pathlib import Path
import stat
import sys
import tempfile
import unittest
from unittest import mock


SCRIPT = Path(__file__).resolve().parents[1] / "scripts" / "extract_tehran.py"
spec = importlib.util.spec_from_file_location("extract_tehran", SCRIPT)
extract_tehran = importlib.util.module_from_spec(spec)
spec.loader.exec_module(extract_tehran)


class ExtractTehranTest(unittest.TestCase):
    def test_both_published_files_are_world_readable_with_restrictive_umask(self):
        with tempfile.TemporaryDirectory() as directory:
            source = Path(directory) / "iran.osm.pbf"
            target = Path(directory) / "tehran.osm.pbf"
            sidecar = Path(str(target) + ".provenance.json")
            source.write_bytes(b"synthetic source")

            def fake_osmium(*args):
                if args[1] == "extract":
                    Path(args[args.index("--output") + 1]).write_bytes(b"synthetic extract")
                if "--get" in args:
                    return "1"
                if "--json" in args:
                    return "{}"
                if "--version" in args:
                    return "osmium mock"
                return ""

            original_umask = os.umask(0o077)
            try:
                with mock.patch.object(extract_tehran.shutil, "which", return_value="/mock/osmium"), \
                     mock.patch.object(extract_tehran, "run", side_effect=fake_osmium), \
                     mock.patch.object(sys, "argv", [str(SCRIPT), "--input", str(source), "--output", str(target)]):
                    extract_tehran.main()
            finally:
                os.umask(original_umask)

            for path in (target, sidecar):
                with self.subTest(path=path.name):
                    self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o644)
            self.assertEqual(json.loads(sidecar.read_text())["output_sha256"], extract_tehran.digest(target))


if __name__ == "__main__":
    unittest.main()
