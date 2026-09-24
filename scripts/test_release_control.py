import json
import io
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import MagicMock, patch

import release_control as release


class ReleaseTests(unittest.TestCase):
    def test_version_and_immutable_prefix(self):
        release.check_release("v0.7.100", "0.7.99", False)
        for tag, stable, occupied in (
            ("v0.7.99", "0.7.100", False),
            ("v0.7.100", "0.7.100", False),
            ("v0.7.101", "0.7.100", True),
            ("v0.7.101-rc.1", "0.7.100", False),
            ("0.7.101", "0.7.100", False),
            ("v00.7.101", "0.7.100", False),
        ):
            with self.subTest(tag=tag, occupied=occupied), self.assertRaises(ValueError):
                release.check_release(tag, stable, occupied)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.names = [
            "arkex_0.7.33_darwin_amd64.tar.gz", "arkex_0.7.33_darwin_arm64.tar.gz",
            "arkex_0.7.33_linux_amd64.tar.gz", "arkex_0.7.33_linux_arm64.tar.gz",
            "arkex_0.7.33_windows_amd64.zip", "SHA256SUMS", "SHA256SUMS.sig",
        ]
        for name in self.names:
            (self.directory / name).write_bytes(name.encode())
        self.remote = None
        self.assets = {}
        self.writes = []
        self.other_releases = []

    def fake_gh(self, *args):
        if args[:2] == ("api", "--paginate"):
            return json.dumps([self.other_releases + ([self.remote] if self.remote else [])])
        if args[0] == "api":
            # GitHub's get-by-tag endpoint does not resolve unpublished drafts.
            raise subprocess.CalledProcessError(1, ["gh", *args], stderr="HTTP 404")
        action = args[1]
        if action == "download":
            name = args[args.index("--pattern") + 1]
            directory = Path(args[args.index("--dir") + 1])
            (directory / name).write_bytes(self.assets[name])
            return ""
        self.writes.append(args)
        if action == "create":
            self.remote = {"tag_name": "v0.7.33", "draft": True, "assets": [], "prerelease": False}
        elif action == "upload":
            path = Path(args[3])
            self.assets[path.name] = path.read_bytes()
            self.remote["assets"].append({"name": path.name})
        elif action == "edit":
            self.remote["draft"] = False
        else:
            self.fail(f"Unexpected gh action: {args}")
        return ""

    def test_initial_publish_then_rerun_changes_nothing(self):
        with patch.object(release, "gh", side_effect=self.fake_gh):
            release.github("v0.7.33", self.directory)
            self.assertEqual([call[1] for call in self.writes], ["create"] + ["upload"] * 7 + ["edit"])
            self.assertEqual(set(self.assets), set(self.names))
            self.assertIn("--latest", self.writes[-1])
            self.writes.clear()
            release.github("v0.7.33", self.directory)
            self.assertEqual(self.writes, [])

    def test_partial_upload_resumes_without_clobber(self):
        def fail_upload(*args):
            if args[:2] == ("release", "upload") and len(self.assets) == 2:
                raise subprocess.CalledProcessError(1, ["gh"])
            return self.fake_gh(*args)
        with patch.object(release, "gh", side_effect=fail_upload), self.assertRaises(subprocess.CalledProcessError):
            release.github("v0.7.33", self.directory)
        self.assertTrue(self.remote["draft"])
        self.assertEqual(len(self.assets), 2)
        self.writes.clear()
        self.other_releases = [{"tag_name": "v0.8.0", "draft": False, "prerelease": False}]
        with patch.object(release, "gh", side_effect=self.fake_gh):
            release.github("v0.7.33", self.directory)
        self.assertEqual([call[1] for call in self.writes], ["upload"] * 5 + ["edit"])
        self.assertIn("--latest=false", self.writes[-1])

    def test_mismatched_asset_is_never_overwritten(self):
        self.remote = {"tag_name": "v0.7.33", "draft": True, "assets": [{"name": self.names[0]}]}
        self.assets[self.names[0]] = b"different build"
        with patch.object(release, "gh", side_effect=self.fake_gh), self.assertRaisesRegex(ValueError, "differs"):
            release.github("v0.7.33", self.directory)
        self.assertEqual(self.writes, [])

    def test_missing_local_or_published_assets_fail_closed(self):
        self.remote = {"tag_name": "v0.7.33", "draft": False, "assets": []}
        with patch.object(release, "gh", side_effect=self.fake_gh), self.assertRaisesRegex(ValueError, "incomplete"):
            release.github("v0.7.33", self.directory)
        self.assertEqual(self.writes, [])
        (self.directory / "SHA256SUMS.sig").unlink()
        with patch.object(release, "gh") as api, self.assertRaisesRegex(ValueError, "Missing"):
            release.github("v0.7.33", self.directory)
        api.assert_not_called()

    def test_api_error_does_not_create_release(self):
        with patch.object(release, "gh", side_effect=subprocess.CalledProcessError(1, ["gh"])) as api:
            with self.assertRaises(subprocess.CalledProcessError):
                release.github("v0.7.33", self.directory)
        self.assertEqual(api.call_count, 1)

    def test_missing_setting_does_not_touch_network_or_sign(self):
        with patch.dict(release.os.environ, {}, clear=True), patch.object(release.subprocess, "run") as run:
            with self.assertRaisesRegex(ValueError, "ARKEX_SIGNING_KEY"):
                release.check()
        run.assert_not_called()

    def test_preflight_only_reads_and_rejects_existing_prefix(self):
        settings = {
            "ARKEX_SIGNING_KEY": "test-key", "ARKEX_DOWNLOAD_BASE": "https://get.arkex.dev",
            "ARKEX_R2_BUCKET": "test-bucket",
            "ARKEX_R2_ENDPOINT": "https://example.r2.cloudflarestorage.com",
            "AWS_ACCESS_KEY_ID": "test-id", "AWS_SECRET_ACCESS_KEY": "test-secret",
        }
        boto = MagicMock()
        client = boto.client.return_value
        client.get_object.side_effect = lambda **kwargs: {"Body": io.BytesIO(b'{"version":"0.7.32"}')}
        client.list_objects_v2.return_value = {}
        with patch.dict(release.os.environ, settings, clear=True), patch.object(release.subprocess, "run"), \
                patch.dict(release.sys.modules, {"boto3": boto, "botocore.config": MagicMock()}):
            release.check()
            self.assertEqual([c[0] for c in client.method_calls], ["get_object", "list_objects_v2"])
            client.list_objects_v2.assert_called_with(Bucket="test-bucket", Prefix="", MaxKeys=1)
            client.list_objects_v2.return_value = {"Contents": [{"Key": "v0.7.33/SHA256SUMS"}]}
            with self.assertRaisesRegex(ValueError, "prefix"):
                release.check("v0.7.33")
            client.list_objects_v2.assert_called_with(Bucket="test-bucket", Prefix="v0.7.33/", MaxKeys=1)
            client.get_object.side_effect = RuntimeError("test-secret must not appear in errors")
            with self.assertRaises(ValueError) as error:
                release.check()
            self.assertNotIn("test-secret", str(error.exception))


if __name__ == "__main__":
    unittest.main()
