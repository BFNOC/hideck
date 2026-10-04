#!/usr/bin/env python3
"""Build a Docker-based fnOS package with the official fnpack tool."""

import argparse
import hashlib
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


PACKAGING = Path(__file__).resolve().parent
ROOT = PACKAGING.parents[1]
DEFAULT_HTTP_PORT = 7575
MAX_PORT = 65535
BUILD_TIMEOUT_SECONDS = 60
VERSION_PATTERN = re.compile(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9][A-Za-z0-9.-]*)?")


def release_version(value):
    version = value.removeprefix("v")
    if not VERSION_PATTERN.fullmatch(version):
        raise argparse.ArgumentTypeError("Use a numeric release version, such as 2.1.23; latest/dev are not releases")
    return version


def http_port(value):
    try:
        port = int(value)
    except ValueError as error:
        raise argparse.ArgumentTypeError("HTTP port must be an integer") from error
    if not 1 <= port <= MAX_PORT:
        raise argparse.ArgumentTypeError("HTTP port must be between 1 and 65535")
    return port


def prepare_package(destination, *, version, port):
    shutil.copytree(PACKAGING / "package", destination)
    defaults = destination / "app/defaults"
    defaults.mkdir()
    shutil.copyfile(ROOT / "config/config.example.yaml", defaults / "config.example.yaml")
    shutil.copyfile(ROOT / "LICENSE", destination / "LICENSE")
    for relative in ("manifest", "app/ui/config", "app/docker/docker-compose.yaml"):
        path = destination / relative
        content = path.read_text().replace("@VERSION@", version).replace("@HTTP_PORT@", str(port))
        path.write_text(content)
    for directory in (destination / "cmd", destination / "app/bin"):
        for script in directory.iterdir():
            script.chmod(0o755)
    images = destination / "app/ui/images"
    images.mkdir()
    for size, name in ((64, "ICON.PNG"), (256, "ICON_256.PNG")):
        source = PACKAGING / "assets" / name
        shutil.copyfile(source, destination / name)
        shutil.copyfile(source, images / f"icon_{size}.png")


def build_package(options):
    tool = shutil.which(options.fnpack)
    if tool is None:
        raise FileNotFoundError("fnpack not found; install the official fnpack tool or pass --fnpack")
    output = options.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    artifact = output / f"hideck_{options.version}_fnos.fpk"
    checksums = artifact.with_suffix(".fpk.sha256")
    if artifact.exists() or checksums.exists():
        raise FileExistsError(f"Refusing to replace an existing package or checksum: {artifact}")
    with tempfile.TemporaryDirectory(prefix="hideck-fnos-") as directory:
        stage = Path(directory) / "hideck"
        prepare_package(stage, version=options.version, port=options.http_port)
        subprocess.run([tool, "build", "--directory", str(stage)], cwd=directory,
                       check=True, timeout=BUILD_TIMEOUT_SECONDS)
        produced = list(Path(directory).rglob("*.fpk"))
        if len(produced) != 1 or produced[0].stat().st_size == 0:
            raise RuntimeError("fnpack did not produce exactly one nonempty FPK")
        # Exclusive create protects an output concurrently produced by another build.
        with produced[0].open("rb") as source, artifact.open("xb") as target:
            shutil.copyfileobj(source, target)
    digest = hashlib.sha256(artifact.read_bytes()).hexdigest()
    with checksums.open("x") as target:
        target.write(f"{digest}  {artifact.name}\n")
    print(f"Built {artifact}; requires published image yibaiba/hideck:{options.version}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True, type=release_version)
    parser.add_argument("--http-port", type=http_port, default=DEFAULT_HTTP_PORT)
    parser.add_argument("--fnpack", default="fnpack")
    parser.add_argument("--output", type=Path, required=True)
    options = parser.parse_args()
    try:
        build_package(options)
    except (OSError, RuntimeError, subprocess.SubprocessError) as error:
        parser.exit(1, f"fnOS package build failed: {error}\n")


if __name__ == "__main__":
    main()
