#!/usr/bin/env python3
"""Tests for the Arena command bus watcher. No network, no real issues.

    python -m unittest discover -s scripts -p "test_arena_watcher.py"
"""

import json
import shutil
import sys
import tempfile
import unittest
from pathlib import Path

try:
    import arena_watcher as aw
except ModuleNotFoundError:  # running from the repository root
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    import arena_watcher as aw


# --------------------------------------------------------------------------
# Fixtures
# --------------------------------------------------------------------------


def command_body(**over):
    data = {"id": "CMD-0001", "action": "git_status", "args": {}, "timeout_s": 60, "note": ""}
    data.update(over)
    return "Команда для станции:\n\n```json\n" + json.dumps(data, ensure_ascii=False) + "\n```\n"


def make_issue(number=1, body="", labels=(aw.LABEL_RUN,), login="owner",
               association="OWNER", updated="2026-11-10T08:00:00Z"):
    return {
        "number": number,
        "body": body,
        "labels": [{"name": name} for name in labels],
        "user": {"login": login},
        "author_association": association,
        "created_at": updated,
        "updated_at": updated,
    }


class FakeGh:
    """The slice of GhClient the watcher calls, recorded instead of sent."""

    def __init__(self, issues=()):
        self.issues = {int(item["number"]): json.loads(json.dumps(item)) for item in issues}
        self.calls = []
        self.get_hook = None

    def list_issues(self, labels):
        wanted = set(labels)
        return [json.loads(json.dumps(item)) for item in self.issues.values()
                if wanted & aw.label_names(item)]

    def get_issue(self, number):
        if self.get_hook is not None:
            self.get_hook(self.issues[int(number)])
        return json.loads(json.dumps(self.issues[int(number)]))

    def comment(self, number, body):
        self.calls.append(("comment", int(number), body))

    def add_label(self, number, label):
        self.calls.append(("add_label", int(number), label))
        names = aw.label_names(self.issues[int(number)])
        names.add(label)
        self.issues[int(number)]["labels"] = [{"name": name} for name in sorted(names)]

    def remove_label(self, number, label):
        self.calls.append(("remove_label", int(number), label))
        names = aw.label_names(self.issues[int(number)])
        names.discard(label)
        self.issues[int(number)]["labels"] = [{"name": name} for name in sorted(names)]

    def create_labels(self):
        self.calls.append(("create_labels",))

    def kinds(self):
        return [call[0] for call in self.calls]


class FakeRunner:
    def __init__(self, code=0, stdout="", stderr=""):
        self.code = code
        self.stdout = stdout
        self.stderr = stderr
        self.calls = []

    def __call__(self, argv, cwd=None, timeout=None, stdin_text=None, env=None):
        self.calls.append({"argv": list(argv), "cwd": cwd, "timeout": timeout, "env": env})
        return aw.Completed(self.code, self.stdout, self.stderr)


def config(tmp, **over):
    raw = {
        "repo": "owner/name",
        "workdir": str(tmp),
        "allowed_logins": ["owner"],
        "allow": {"log_dirs": ["logs"], "default_test_packages": ["./internal/bridge"]},
    }
    raw.update(over)
    return aw.Config.from_dict(raw)


class WatcherCase(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        (self.tmp / "logs").mkdir()
        self.cfg = config(self.tmp)
        self.log = []

    def watcher(self, gh, runner=None, **kw):
        return aw.Watcher(self.cfg, gh, runner=runner or FakeRunner(),
                          logger=lambda event, fields: self.log.append((event, fields)), **kw)

    def tearDown(self):
        shutil.rmtree(self.tmp, ignore_errors=True)


# --------------------------------------------------------------------------
# Protocol
# --------------------------------------------------------------------------


class ParseTest(WatcherCase):
    def test_valid_command(self):
        cmd, error = aw.parse_command(command_body(action="git_log", args={"n": 5}), self.cfg)
        self.assertEqual(error, "")
        self.assertEqual((cmd.id, cmd.action, cmd.timeout_s), ("CMD-0001", "git_log", 60))
        self.assertEqual(cmd.args, {"n": 5})
        self.assertEqual(len(cmd.sha), 64)

    def test_missing_block(self):
        cmd, error = aw.parse_command("Просто текст без команды", self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("no ```json command block", error)

    def test_two_blocks_are_refused(self):
        body = command_body() + "\n" + command_body(id="CMD-0002")
        cmd, error = aw.parse_command(body, self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("more than one command block", error)

    def test_unknown_action(self):
        cmd, error = aw.parse_command(command_body(action="exec", args={"cmd": "rm -rf /"}), self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("not in the whitelist", error)

    def test_unknown_keys(self):
        body = command_body()
        body = body.replace('"note": ""', '"note": "", "shell": true')
        cmd, error = aw.parse_command(body, self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("unknown keys: shell", error)

    def test_duplicate_keys(self):
        body = "```json\n{\"id\": \"CMD-0001\", \"action\": \"git_status\", \"action\": \"build\"}\n```"
        cmd, error = aw.parse_command(body, self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("duplicate key", error)

    def test_bad_command_id(self):
        cmd, error = aw.parse_command(command_body(id="1"), self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("CMD-0001", error)

    def test_timeout_is_clamped(self):
        cmd, _ = aw.parse_command(command_body(timeout_s=99999), self.cfg)
        self.assertEqual(cmd.timeout_s, self.cfg.max_timeout_s)

    def test_body_limit(self):
        cmd, error = aw.parse_command(command_body(note="x" * (self.cfg.max_body_bytes + 10)), self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("the limit is", error)

    def test_fenced_block_without_json_tag(self):
        body = "```\n{\"id\": \"CMD-0003\", \"action\": \"build\"}\n```"
        cmd, error = aw.parse_command(body, self.cfg)
        self.assertEqual(error, "")
        self.assertEqual(cmd.action, "build")


class ArgsTest(WatcherCase):
    def test_read_log_traversal(self):
        for path in ("../../secrets.json", "logs/../../secrets.json", "/etc/passwd", "C:/Windows/win.ini"):
            cmd, error = aw.parse_command(command_body(action="read_log", args={"path": path}), self.cfg)
            self.assertIsNone(cmd, path)
            self.assertTrue(error, path)

    def test_read_log_outside_allowed_dir(self):
        cmd, error = aw.parse_command(command_body(action="read_log", args={"path": "cmd/main.go"}), self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("path must be under", error)

    def test_read_log_allowed(self):
        cmd, error = aw.parse_command(command_body(action="read_log",
                                                   args={"path": "logs/bridge.log", "lines": 20}), self.cfg)
        self.assertEqual(error, "")
        self.assertEqual(cmd.args["lines"], 20)

class GrepTest(WatcherCase):
    def test_glob_must_be_configured(self):
        cmd, error = aw.parse_command(command_body(action="grep", args={"pattern": "TODO", "glob": "*.pem"}),
                                      self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("glob must be one of", error)

    def test_bad_regex(self):
        cmd, error = aw.parse_command(command_body(action="grep", args={"pattern": "["}), self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("not a valid regex", error)

    def test_grep_finds_and_limits(self):
        (self.tmp / "notes.md").write_text("TODO один\nTODO два\nTODO три\n", encoding="utf-8")
        gh = FakeGh()
        watcher = self.watcher(gh)
        cmd, _ = aw.parse_command(command_body(action="grep",
                                              args={"pattern": "TODO", "glob": "*.md", "max": 2}), self.cfg)
        code, out = watcher.execute(cmd)
        self.assertEqual(code, 0)
        self.assertIn("notes.md:1", out)
        self.assertIn("лимит 2 совпадений", out)


class TestsAndBuildTest(WatcherCase):
    def test_package_must_match_allowlist(self):
        for package in ("all", "./etc/passwd", "../../internal/bridge", "./internal/bridge; rm -rf /"):
            cmd, error = aw.parse_command(command_body(action="run_tests", args={"packages": [package]}),
                                          self.cfg)
            self.assertIsNone(cmd, package)

    def test_package_from_regex_is_accepted(self):
        cmd, error = aw.parse_command(command_body(action="run_tests",
                                                  args={"packages": ["./internal/bridge"]}), self.cfg)
        self.assertEqual(error, "")
        runner = FakeRunner(stdout="ok  quasarbridge/internal/bridge")
        watcher = self.watcher(FakeGh(), runner)
        code, out = watcher.execute(cmd)
        self.assertEqual(code, 0)
        self.assertEqual(runner.calls[0]["argv"][1:], ["test", "./internal/bridge"])
        self.assertIn("ok", out)

    def test_build_writes_outside_the_tree(self):
        cmd, _ = aw.parse_command(command_body(action="build"), self.cfg)
        runner = FakeRunner()
        self.watcher(FakeGh(), runner).execute(cmd)
        argv = runner.calls[0]["argv"]
        self.assertEqual(argv[1:3], ["build", "-o"])
        self.assertIn(str(self.cfg.state / "build"), argv[3])


class DeployCheckTest(WatcherCase):
    def test_url_must_be_configured(self):
        cmd, error = aw.parse_command(command_body(action="deploy_check",
                                                  args={"url": "http://example.invalid/healthz"}), self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("configured health entries", error)

    def test_no_health_configured(self):
        cmd, _ = aw.parse_command(command_body(action="deploy_check"), self.cfg)
        code, out = self.watcher(FakeGh()).execute(cmd)
        self.assertEqual(code, 2)
        self.assertIn("no health entries", out)


# --------------------------------------------------------------------------
# Trust, dedup, claim
# --------------------------------------------------------------------------


class TrustTest(WatcherCase):
    def test_foreign_author_is_not_executed_and_not_answered(self):
        gh = FakeGh([make_issue(body=command_body(), login="stranger")])
        runner = FakeRunner()
        outcomes = self.watcher(gh, runner).poll_once()
        self.assertEqual(outcomes[0].kind, "untrusted")
        self.assertEqual(runner.calls, [])
        self.assertNotIn("comment", gh.kinds())
        self.assertIn("remove_label", gh.kinds())
        self.assertNotIn(aw.LABEL_RUN, aw.label_names(gh.issues[1]))

    def test_lower_association_is_not_trusted(self):
        gh = FakeGh([make_issue(body=command_body(), association="CONTRIBUTOR")])
        outcomes = self.watcher(gh).poll_once()
        self.assertEqual(outcomes[0].kind, "untrusted")

    def test_allowed_login_is_case_insensitive(self):
        self.cfg = config(self.tmp, allowed_logins=["Owner"])
        gh = FakeGh([make_issue(body=command_body(), login="owner")])
        outcomes = self.watcher(gh).poll_once()
        self.assertEqual(outcomes[0].kind, "done")


class DedupTest(WatcherCase):
    def test_running_issue_is_not_executed_twice(self):
        gh = FakeGh([make_issue(body=command_body(), labels=(aw.LABEL_RUNNING,))])
        runner = FakeRunner()
        outcomes = self.watcher(gh, runner).poll_once()
        self.assertEqual(outcomes[0].kind, "skipped")
        self.assertEqual(runner.calls, [])
        self.assertEqual(gh.calls, [])

    def test_paused_label_skips(self):
        gh = FakeGh([make_issue(body=command_body(), labels=(aw.LABEL_RUN, aw.LABEL_PAUSED))])
        runner = FakeRunner()
        outcomes = self.watcher(gh, runner).poll_once()
        self.assertEqual(outcomes[0].kind, "skipped")
        self.assertEqual(runner.calls, [])

    def test_stale_running_is_failed_without_retry(self):
        gh = FakeGh([make_issue(labels=(aw.LABEL_RUNNING,), updated="2026-11-10T06:00:00Z")])
        runner = FakeRunner()
        watcher = self.watcher(gh, runner, now=lambda: aw.parse_time("2026-11-10T08:00:00Z"))
        outcomes = watcher.poll_once()
        self.assertEqual(outcomes[0].kind, "stale")
        self.assertEqual(runner.calls, [])
        self.assertIn("comment", gh.kinds())
        self.assertIn(aw.LABEL_FAILED, aw.label_names(gh.issues[1]))
        self.assertNotIn(aw.LABEL_RUNNING, aw.label_names(gh.issues[1]))

    def test_body_changed_during_claim_is_refused(self):
        gh = FakeGh([make_issue(body=command_body())])

        def mutate(issue):
            issue["body"] = command_body(action="build")

        gh.get_hook = mutate
        runner = FakeRunner()
        outcomes = self.watcher(gh, runner).poll_once()
        self.assertEqual(outcomes[0].kind, "rejected")
        self.assertEqual(runner.calls, [])
        comment = [call for call in gh.calls if call[0] == "comment"][0][2]
        self.assertIn("body changed", comment)
        self.assertIn(aw.LABEL_FAILED, aw.label_names(gh.issues[1]))


# --------------------------------------------------------------------------
# Round in full
# --------------------------------------------------------------------------


class RoundTest(WatcherCase):
    def test_successful_round(self):
        gh = FakeGh([make_issue(body=command_body())])
        runner = FakeRunner(stdout="## main...origin/main\n M cmd/main.go")
        outcomes = self.watcher(gh, runner).poll_once()
        self.assertEqual(outcomes[0].kind, "done")
        self.assertEqual(runner.calls[0]["argv"], ["git", "status", "--porcelain=v1", "--branch"])
        self.assertEqual(runner.calls[0]["timeout"], 60)
        self.assertNotIn("GH_TOKEN", runner.calls[0]["env"])
        labels = aw.label_names(gh.issues[1])
        self.assertEqual(labels, {aw.LABEL_DONE})
        comment = [call for call in gh.calls if call[0] == "comment"][0][2]
        self.assertIn("статус: **ок**", comment)
        self.assertIn("M cmd/main.go", comment)
        self.assertIn(aw.body_sha(command_body())[:16], comment)
        self.assertLess(gh.kinds().index("add_label"), gh.kinds().index("comment"))

    def test_failed_action_marks_issue_failed(self):
        gh = FakeGh([make_issue(body=command_body())])
        runner = FakeRunner(code=2, stderr="fatal: not a git repository")
        outcomes = self.watcher(gh, runner).poll_once()
        self.assertEqual(outcomes[0].kind, "failed")
        self.assertEqual(aw.label_names(gh.issues[1]), {aw.LABEL_FAILED})
        comment = [call for call in gh.calls if call[0] == "comment"][0][2]
        self.assertIn("ошибка", comment)
        self.assertIn("not a git repository", comment)

    def test_rejected_command_is_reported_without_running(self):
        gh = FakeGh([make_issue(body=command_body(action="deploy"))])
        runner = FakeRunner()
        outcomes = self.watcher(gh, runner).poll_once()
        self.assertEqual(outcomes[0].kind, "rejected")
        self.assertEqual(runner.calls, [])
        comment = [call for call in gh.calls if call[0] == "comment"][0][2]
        self.assertIn("отклонено", comment)
        self.assertIn("whitelist", comment)
        self.assertEqual(aw.label_names(gh.issues[1]), {aw.LABEL_FAILED})

    def test_dry_run_changes_nothing(self):
        gh = FakeGh([make_issue(body=command_body())])
        runner = FakeRunner()
        outcomes = self.watcher(gh, runner, dry_run=True).poll_once()
        self.assertEqual(outcomes[0].kind, "dry-run")
        self.assertEqual(gh.calls, [])
        self.assertEqual(runner.calls, [])
        self.assertEqual(aw.label_names(gh.issues[1]), {aw.LABEL_RUN})

    def test_output_is_clipped_and_redacted_in_the_comment(self):
        gh = FakeGh([make_issue(body=command_body())])
        runner = FakeRunner(stdout="token=SUPERSECRET\n" + "line\n" * 500)
        self.watcher(gh, runner).poll_once()
        comment = [call for call in gh.calls if call[0] == "comment"][0][2]
        self.assertNotIn("SUPERSECRET", comment)
        self.assertIn("обрезано", comment)


# --------------------------------------------------------------------------
# Text helpers
# --------------------------------------------------------------------------


class RedactTest(unittest.TestCase):
    def test_keyword_values_keep_their_key(self):
        self.assertEqual(aw.redact("token=SUPERSECRET"), "token=***")
        self.assertEqual(aw.redact("Authorization: Bearer abc123"), "Authorization: ***")
        self.assertEqual(aw.redact("X-Api-Key: sk-livevalue"), "X-Api-Key: ***")
        self.assertIn("password=***", aw.redact("password=hunter2"))

    def test_tokens(self):
        self.assertEqual(aw.redact("ghp_" + "A" * 10), "***")
        self.assertEqual(aw.redact("github_pat_" + "B" * 10), "***")

    def test_private_key_block(self):
        key = "-----BEGIN " + "RSA PRIVATE KEY-----\nMIIB\n-----END " + "RSA PRIVATE KEY-----"
        self.assertEqual(aw.redact(key), "***")

    def test_plain_text_untouched(self):
        self.assertEqual(aw.redact("exit=0, 12 строк"), "exit=0, 12 строк")


class ClipTest(unittest.TestCase):
    def test_lines(self):
        out = aw.clip("a\nb\nc\nd", max_lines=2)
        self.assertTrue(out.startswith("a\nb"))
        self.assertIn("обрезано", out)

    def test_bytes(self):
        out = aw.clip("я" * 100, max_bytes=20)
        self.assertTrue(out.startswith("я" * 10))  # 20 bytes = 10 two-byte letters
        self.assertIn("обрезано", out)
        self.assertNotIn("я" * 11, out)

    def test_short_text_untouched(self):
        self.assertEqual(aw.clip("ok", 100, 10), "ok")


class LabelTest(unittest.TestCase):
    def test_rest_shape_and_plain_names(self):
        self.assertEqual(aw.label_names({"labels": [{"name": "oc:run"}]}), {"oc:run"})
        self.assertEqual(aw.label_names({"labels": ["oc:done"]}), {"oc:done"})
        self.assertEqual(aw.label_names({}), set())

    def test_issue_author(self):
        login, association = aw.issue_author({"user": {"login": "x"}, "author_association": "owner"})
        self.assertEqual((login, association), ("x", "OWNER"))


class ConfigTest(unittest.TestCase):
    def test_example_config_parses(self):
        path = Path(__file__).resolve().parent / "arena_watcher.example.json"
        cfg = aw.Config.load(path)
        self.assertEqual(cfg.repo, "owner/name")
        self.assertEqual(cfg.poll_s, 60)
        self.assertEqual(len(cfg.health), 2)
        self.assertTrue(all(entry["label"] for entry in cfg.health))

    def test_login_allowlist_is_required(self):
        with self.assertRaises(aw.ConfigError):
            aw.Config.from_dict({"repo": "owner/name", "workdir": ".", "allowed_logins": []})

    def test_bad_repo(self):
        with self.assertRaises(aw.ConfigError):
            aw.Config.from_dict({"repo": "no-slash", "workdir": ".", "allowed_logins": ["x"]})

    def test_bad_regex(self):
        with self.assertRaises(aw.ConfigError):
            aw.Config.from_dict({"repo": "o/n", "workdir": ".", "allowed_logins": ["x"],
                                 "allow": {"test_package_re": "("}})


if __name__ == "__main__":
    unittest.main()
