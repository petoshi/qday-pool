#!/usr/bin/env python3
"""Build one native QDAY Pool operator archive."""

import argparse
import hashlib
import os
import pathlib
import platform
import shutil
import subprocess
import tarfile


ROOT = pathlib.Path(__file__).resolve().parents[1]
ARCHES = {"x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64", "arm64": "arm64"}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--go", default="go")
    parser.add_argument("--output", type=pathlib.Path, default=ROOT / "build" / "dist")
    args = parser.parse_args()

    version = args.version.removeprefix("v")
    if not version or any(c not in "0123456789." for c in version):
        parser.error("version must contain decimal components")
    if platform.system() != "Linux" or platform.machine() not in ARCHES:
        parser.error("release packages are built natively on Linux x86-64 or ARM64")
    target = f"linux-{ARCHES[platform.machine()]}"
    name = f"QDAY-Pool-{version}-{target}"
    stage = args.output.resolve() / name
    shutil.rmtree(stage, ignore_errors=True)
    stage.mkdir(parents=True)

    env = os.environ.copy()
    env.update(CGO_ENABLED="1", GOOS="linux", GOARCH=target.removeprefix("linux-"))
    tags = "netgo,osusergo,sqlite_omit_load_extension"
    ldflags = f"-s -w -linkmode=external -extldflags=-static -X main.version={version}"
    subprocess.run(
        [args.go, "build", "-trimpath", "-tags", tags, "-ldflags", ldflags,
         "-o", str(stage / "qday-pool"), "./cmd/qday-pool"],
        cwd=ROOT, env=env, check=True,
    )
    (stage / "qday-pool").chmod(0o755)
    for filename in ("README.md", "LICENSE"):
        shutil.copy2(ROOT / filename, stage / filename)
    shutil.copytree(ROOT / "docs", stage / "docs")
    shutil.copytree(ROOT / "deploy", stage / "deploy")
    shutil.copytree(ROOT / "licenses", stage / "licenses")
    (stage / "START-HERE.txt").write_text(
        f"""QDAY POOL {version} — {target}

This is the public PPLNS pool controller. It does not contain a wallet,
seed phrase, API token, blockchain state or miner.

Requirements:
- QDAY Node v0.8.1 or newer on the same machine
- an unlocked pool payout wallet
- the node API bound to loopback

Read docs/operations.md before starting the service. Expose TCP 3333 for
SiaMining Stratum and publish the dashboard through HTTPS. Never expose the
QDAY API on port 19770.
""",
        encoding="utf-8",
    )

    archive = args.output.resolve() / f"{name}.tar.gz"
    with tarfile.open(archive, "w:gz") as bundle:
        bundle.add(stage, arcname=name)
    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    pathlib.Path(str(archive) + ".sha256").write_text(f"{digest}  {archive.name}\n", encoding="utf-8")
    try:
        display_path = archive.relative_to(ROOT)
    except ValueError:
        display_path = archive
    print(display_path)


if __name__ == "__main__":
    main()
