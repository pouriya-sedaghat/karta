#!/usr/bin/env python3
"""Create a reproducible Chitgar-area OSM development extract from an Iran PBF."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


# West, south, east, north. Includes Chitgar Lake, the surrounding park,
# arterial and local roads, and the Iran Mall area. Coordinates are WGS84.
DEFAULT_BBOX = "51.175,35.705,51.285,35.785"


def digest(path):
    value = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            value.update(chunk)
    return value.hexdigest()


def run(*args):
    return subprocess.run(args, check=True, text=True, capture_output=True).stdout


def bbox_arg(value):
    try:
        west, south, east, north = map(float, value.split(","))
        if not (-180 <= west < east <= 180 and -90 <= south < north <= 90):
            raise ValueError("invalid coordinate order/range")
    except ValueError as error:
        raise argparse.ArgumentTypeError("bbox must be west,south,east,north in WGS84") from error
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, required=True, help="local Iran .osm.pbf snapshot")
    parser.add_argument("--output", type=Path, required=True, help="new local .osm.pbf extract")
    parser.add_argument("--bbox", type=bbox_arg, default=DEFAULT_BBOX)
    parser.add_argument("--expected-sha256", help="optional published SHA-256 of the input PBF")
    args = parser.parse_args()
    source = args.input.absolute()
    target = args.output.absolute()
    sidecar = Path(str(target) + ".provenance.json")
    if not source.is_file() or source.is_symlink() or not source.name.endswith(".osm.pbf"):
        parser.error("input must be an existing, regular .osm.pbf file (not a symlink)")
    if target.suffix != ".pbf" or not target.name.endswith(".osm.pbf"):
        parser.error("output must end with .osm.pbf")
    if source == target or target.exists() or target.is_symlink() or sidecar.exists():
        parser.error("output must be different from input and neither output nor sidecar may exist")
    if not target.parent.is_dir():
        parser.error("output directory must already exist")
    if shutil.which("osmium") is None:
        parser.error("osmium-tool is required; install it and put osmium on PATH")
    if args.expected_sha256 and (len(args.expected_sha256) != 64 or
                                 any(c not in "0123456789abcdefABCDEF" for c in args.expected_sha256)):
        parser.error("expected SHA-256 must be 64 hexadecimal characters")

    temporary = None
    sidecar_temporary = None
    try:
        # The temporary file needs the PBF suffix for osmium's format detection.
        with tempfile.NamedTemporaryFile(prefix=".tehran-", suffix=".osm.pbf", dir=target.parent, delete=False) as handle:
            temporary = Path(handle.name)
        source_sha = digest(source)
        if args.expected_sha256 and source_sha != args.expected_sha256.lower():
            raise ValueError("source SHA-256 differs from --expected-sha256")
        source_info = json.loads(run("osmium", "fileinfo", "--extended", "--json", str(source)))
        run("osmium", "extract", "--bbox", args.bbox, "--strategy", "smart",
            "--set-bounds", "--fsync", str(source), "--output", str(temporary), "--overwrite")
        if digest(source) != source_sha:
            raise ValueError("input changed during extraction; use a completed snapshot")
        extract_info = json.loads(run("osmium", "fileinfo", "--extended", "--json", str(temporary)))
        if int(run("osmium", "fileinfo", "--extended", "--get", "data.count.nodes", str(temporary)).strip()) == 0:
            raise ValueError("extract contains no nodes; check the source and bounding box")
        run("osmium", "check-refs", str(temporary))  # Ways' nodes; other relations can be partial.
        if temporary.stat().st_size == 0:
            raise ValueError("empty extract")
        metadata = {
            "source": str(source), "source_sha256": source_sha,
            "output_sha256": digest(temporary), "bbox_wgs84": args.bbox,
            "strategy": "smart", "osmium_version": run("osmium", "--version").strip(),
            "source_fileinfo": source_info, "output_fileinfo": extract_info,
            "license": "ODbL 1.0", "attribution": "© OpenStreetMap contributors",
            "license_url": "https://www.openstreetmap.org/copyright",
        }
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", prefix=".tehran-",
                                     suffix=".json", dir=target.parent, delete=False) as handle:
            sidecar_temporary = Path(handle.name)
            json.dump(metadata, handle, indent=2, ensure_ascii=False)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary, 0o644)  # Public OSM data; a containerized importer may run under another UID.
        os.chmod(sidecar_temporary, 0o644)  # The same importer must read the provenance sidecar.
        # Link creation fails if another process has created the destination.
        os.link(temporary, target)
        try:
            os.link(sidecar_temporary, sidecar)
        except OSError:
            target.unlink()
            raise
        print(f"Extract: {target}\nProvenance: {sidecar}\nSHA-256: {metadata['output_sha256']}")
    finally:
        for path in (temporary, sidecar_temporary):
            if path is not None:
                path.unlink(missing_ok=True)


if __name__ == "__main__":
    try:
        main()
    except (OSError, subprocess.CalledProcessError, ValueError) as error:
        print(f"Extraction failed: {error}", file=sys.stderr)
        if isinstance(error, subprocess.CalledProcessError) and error.stderr:
            print(error.stderr, file=sys.stderr)
        sys.exit(1)
