"""Release preflight and resumable GitHub asset publication. Never logs secrets."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from urllib.parse import urlsplit

REPO = "attrition-tech/arkex"


def version(tag):
    if not re.fullmatch(r"v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)", tag):
        raise ValueError("Only stable major.minor.patch versions are supported")
    return tuple(map(int, tag.removeprefix("v").split(".")))


def check_release(tag, stable, occupied):
    if not tag.startswith("v"):
        raise ValueError("Release tag must start with v")
    if version(tag) <= version(stable):
        raise ValueError("Release must be newer than the current stable version; do not rerun R2 publication")
    if occupied:
        raise ValueError("Release prefix already contains files; refusing to overwrite a partial or complete release")


def check(tag=None):
    names = ("ARKEX_SIGNING_KEY", "ARKEX_DOWNLOAD_BASE", "ARKEX_R2_BUCKET",
             "ARKEX_R2_ENDPOINT", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")
    missing = [name for name in names if not os.environ.get(name, "").strip()]
    if missing:
        raise ValueError("Missing settings: " + ", ".join(missing))
    if os.environ["ARKEX_DOWNLOAD_BASE"] != "https://get.arkex.dev":
        raise ValueError("ARKEX_DOWNLOAD_BASE must be https://get.arkex.dev")
    endpoint = urlsplit(os.environ["ARKEX_R2_ENDPOINT"])
    if (endpoint.scheme != "https" or not (endpoint.hostname or "").endswith(".r2.cloudflarestorage.com")
            or endpoint.username or endpoint.password or endpoint.port
            or endpoint.path not in ("", "/") or endpoint.query or endpoint.fragment):
        raise ValueError("ARKEX_R2_ENDPOINT must be the HTTPS Cloudflare R2 S3 endpoint")
    subprocess.run(["go", "run", "./scripts/relsign", "check-key"], check=True)
    import boto3  # Release tooling only, installed before loading credentials.
    from botocore.config import Config
    client = boto3.client("s3", endpoint_url=os.environ["ARKEX_R2_ENDPOINT"],
                          region_name="auto", config=Config(connect_timeout=10, read_timeout=30,
                                                          retries={"max_attempts": 2}))
    bucket = os.environ["ARKEX_R2_BUCKET"]
    try:
        stable = json.loads(client.get_object(Bucket=bucket, Key="stable.json")["Body"].read())
        version(stable["version"])
        # A read-only probe. This does not claim to verify write permission.
        existing = client.list_objects_v2(Bucket=bucket, Prefix=(tag + "/") if tag else "", MaxKeys=1)
    except Exception:
        raise ValueError("Cannot read a valid stable manifest and list the R2 bucket; check R2 settings and permissions") from None
    if tag:
        check_release(tag, stable["version"], bool(existing.get("Contents")))
    print("PASS: required settings, trusted signing key, and R2 read/list access" +
          ("; new version and empty release prefix" if tag else " (no writes performed)"))


def gh(*args):
    return subprocess.check_output(["gh", *args], text=True)


def sha256(path):
    with open(path, "rb") as file:
        return hashlib.file_digest(file, "sha256").hexdigest()


def github(tag, directory):
    version(tag)
    directory = Path(directory)
    ver = tag.removeprefix("v")
    names = [f"arkex_{ver}_{platform}.{extension}" for platform, extension in (
        ("darwin_amd64", "tar.gz"), ("darwin_arm64", "tar.gz"),
        ("linux_amd64", "tar.gz"), ("linux_arm64", "tar.gz"), ("windows_amd64", "zip"))]
    names += ["SHA256SUMS", "SHA256SUMS.sig"]
    if any(not (directory / name).is_file() for name in names):
        raise ValueError("Missing release assets")
    # Listing distinguishes an absent release from authentication/network errors.
    pages = json.loads(gh("api", "--paginate", "--slurp", f"repos/{REPO}/releases?per_page=100"))
    releases = [release for page in pages for release in page]
    release = next((r for r in releases if r["tag_name"] == tag), None)
    if release is None:
        gh("release", "create", tag, "--repo", REPO, "--verify-tag", "--draft",
           "--title", f"Arkex {tag}", "--generate-notes")
        # Get-by-tag only resolves published releases. The authenticated list
        # includes drafts and also lets a failed upload resume on a rerun.
        pages = json.loads(gh("api", "--paginate", "--slurp", f"repos/{REPO}/releases?per_page=100"))
        releases = [r for page in pages for r in page]
        release = next((r for r in releases if r["tag_name"] == tag), None)
        if release is None:
            raise ValueError("Created draft is not visible yet; rerun the GitHub release job")
    existing = {a["name"] for a in release["assets"]}
    with tempfile.TemporaryDirectory(prefix="arkex-release-") as temp:
        for name in names:
            if name in existing:
                gh("release", "download", tag, "--repo", REPO, "--pattern", name, "--dir", temp)
                if sha256(Path(temp) / name) != sha256(directory / name):
                    raise ValueError("Existing GitHub asset differs; refusing to overwrite: " + name)
            elif release["draft"]:
                gh("release", "upload", tag, str(directory / name), "--repo", REPO)
            else:
                raise ValueError("Published GitHub release is incomplete; refusing to alter it")
    if release["draft"]:
        newer = any(not r["draft"] and not r["prerelease"] and
                    version(r["tag_name"]) > version(tag) for r in releases)
        gh("release", "edit", tag, "--repo", REPO, "--draft=false",
           "--latest=false" if newer else "--latest")
    print("PASS: GitHub release published with matching assets")


if __name__ == "__main__":
    try:
        if len(sys.argv) in (2, 3) and sys.argv[1] == "check":
            check(sys.argv[2] if len(sys.argv) == 3 else None)
        elif len(sys.argv) == 4 and sys.argv[1] == "github":
            github(sys.argv[2], sys.argv[3])
        else:
            raise ValueError("Usage: release_control.py check [TAG] | github TAG DIRECTORY")
    except (ValueError, subprocess.CalledProcessError) as error:
        print(f"Release check failed: {error}", file=sys.stderr)
        sys.exit(1)
