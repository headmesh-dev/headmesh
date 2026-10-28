"""Package the validated artwork and complete reconstruction source without installed dependencies."""

import argparse
import hashlib
import json
import os
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, default=ROOT.parents[1] / "headmesh-brand-all-13-reconstructed.zip")
    args = parser.parse_args()
    for filename in ("validation.json", "gallery-validation.json", "png/validation.json", "png/gallery-validation.json"):
        if not json.loads((ROOT / filename).read_text())["passed"]:
            raise SystemExit(f"Refusing to package unsuccessful validation: {filename}")
    prefix = Path("headmesh-brand-reconstructed")
    excluded = {".venv", "node_modules", "tmp", "__pycache__", ".git"}
    included = []
    with zipfile.ZipFile(args.output, "x", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for directory, subdirectories, filenames in os.walk(ROOT):
            subdirectories[:] = sorted(name for name in subdirectories if name not in excluded)
            for name in sorted(filenames):
                path = Path(directory) / name
                if path.suffix == ".pyc":
                    continue
                relative = path.relative_to(ROOT)
                archive.write(path, str(prefix / relative))
                included.append(relative)
    checks = (ROOT / "SHA256SUMS").read_text().splitlines()
    for entry in (ROOT / "png/SHA256SUMS").read_text().splitlines():
        digest, relative = entry.split("  ", 1)
        checks.append(f"{digest}  png/{relative}")
    with zipfile.ZipFile(args.output) as archive:
        bad = archive.testzip()
        if bad:
            raise SystemExit(f"Archive CRC failure: {bad}")
        for entry in checks:
            digest, relative = entry.split("  ", 1)
            if hashlib.sha256(archive.read(str(prefix / relative))).hexdigest() != digest:
                raise SystemExit(f"Packaged asset hash differs: {relative}")
    digest = hashlib.sha256(args.output.read_bytes()).hexdigest()
    args.output.with_suffix(args.output.suffix + ".sha256").write_text(f"{digest}  {args.output.name}\n")
    print(json.dumps({"archive": str(args.output), "files": len(included), "bytes": args.output.stat().st_size,
                      "sha256": digest, "asset_hashes_checked_in_archive": len(checks)}, indent=2))


if __name__ == "__main__":
    main()
