#!/usr/bin/env python3
"""Tests for the Arena command bus watcher. No network, no real issues.

    python -m unittest discover -s scripts -p "test_arena_watcher.py"
"""

import contextlib
import io
import json
import os
import shutil
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

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
               association="OWNER", updated=None, title="[CMD] CMD-0001 git_status"):
    return {
        "number": number,
        "title": title,
        "body": body,
        "labels": [{"name": name} for name in labels],
        "user": {"login": login},
        "author_association": association,
        "created_at": updated or aw.utcnow().isoformat().replace("+00:00", "Z"),
        "updated_at": updated or aw.utcnow().isoformat().replace("+00:00", "Z"),
    }


class FakeGh:
    """The slice of GhClient the watcher calls, recorded instead of sent."""

    def __init__(self, issues=()):
        self.issues = {int(item["number"]): json.loads(json.dumps(item)) for item in issues}
        self.calls = []
        self.get_hook = None

    def list_open_issues(self):
        return [json.loads(json.dumps(item)) for item in self.issues.values()
                if item.get("state", "open") == "open"]

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

    def close_issue(self, number):
        self.calls.append(("close_issue", int(number)))
        self.issues[int(number)]["state"] = "closed"

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

class ArgsTestLimits(WatcherCase):
    def test_allow_limits_reach_the_validator(self):
        self.cfg = config(self.tmp, allow={"log_max_lines": 5, "git_log_max": 5})
        cmd, error = aw.parse_command(command_body(action="read_log",
                                                  args={"path": "logs/bridge.log", "lines": 6}), self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("1..5", error)
        cmd, error = aw.parse_command(command_body(action="git_log", args={"n": 6}), self.cfg)
        self.assertIsNone(cmd)
        self.assertIn("1..5", error)


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
    def test_dotdot_inside_a_matching_package_is_refused(self):
        for package in ("./internal/../../cmd/quasar-bridge", "./internal/bridge/../../..",
                        "./internal/../cmd/quasar-bridge"):
            cmd, error = aw.parse_command(command_body(action="run_tests", args={"packages": [package]}),
                                          self.cfg)
            self.assertIsNone(cmd, package)
            self.assertIn("..", error)

    def test_absolute_package_is_refused(self):
        cmd, error = aw.parse_command(command_body(action="run_tests",
                                                  args={"packages": ["/usr/bin"]}), self.cfg)
        self.assertIsNone(cmd)

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


class TitleTriggerTest(WatcherCase):
    """Arena cannot label anything: the title has to be enough."""

    def test_bracket_title_runs_without_any_label(self):
        gh = FakeGh([make_issue(labels=(), body=command_body(), title="[CMD] git_status")])
        runner = FakeRunner(stdout=" M bridge.go")
        outcomes = self.watcher(gh, runner).poll_once()
        self.assertEqual(outcomes[0].kind, "done")
        self.assertEqual(len(runner.calls), 1)
        self.assertEqual(aw.label_names(gh.issues[1]), {aw.LABEL_DONE})
        self.assertIn("close_issue", gh.kinds())
        # no phantom label removal: the issue never had oc:run
        self.assertNotIn(("remove_label", 1, aw.LABEL_RUN), gh.calls)

    def test_oc_title_with_command_id_runs(self):
        gh = FakeGh([make_issue(labels=(), body=command_body(),
                                title="oc: CMD-0002 git_log n=3 (arena smoke)")])
        outcomes = self.watcher(gh).poll_once()
        self.assertEqual(outcomes[0].kind, "done")

    def test_plain_title_is_ignored(self):
        gh = FakeGh([make_issue(labels=(), title="feat: add a thing")])
        runner = FakeRunner()
        self.assertEqual(self.watcher(gh, runner).poll_once(), [])
        self.assertEqual(runner.calls, [])
        self.assertEqual(gh.calls, [])

    def test_run_label_still_triggers_regardless_of_title(self):
        gh = FakeGh([make_issue(labels=(aw.LABEL_RUN,), body=command_body(), title="что угодно")])
        outcomes = self.watcher(gh).poll_once()
        self.assertEqual(outcomes[0].kind, "done")

    def test_finished_title_issue_is_not_run_again(self):
        gh = FakeGh([make_issue(labels=(aw.LABEL_DONE,), title="[CMD] git_status")])
        runner = FakeRunner()
        self.assertEqual(self.watcher(gh, runner).poll_once(), [])
        self.assertEqual(runner.calls, [])

    def test_title_issue_with_foreign_author_is_silent(self):
        gh = FakeGh([make_issue(labels=(), login="stranger", title="[CMD] git_status")])
        runner = FakeRunner()
        outcomes = self.watcher(gh, runner).poll_once()
        self.assertEqual(outcomes[0].kind, "untrusted")
        self.assertEqual(runner.calls, [])
        self.assertEqual(gh.calls, [])
        self.assertEqual(aw.label_names(gh.issues[1]), set())

    def test_untrusted_reported_once(self):
        gh = FakeGh([make_issue(labels=(), login="stranger", title="[CMD] git_status")])
        watcher = self.watcher(gh)
        watcher.poll_once()
        watcher.poll_once()
        self.assertEqual([event for event, _ in self.log], ["untrusted"])

    def test_bad_body_on_title_issue_is_reported_and_closed(self):
        gh = FakeGh([make_issue(labels=(), body="нет блока", title="[CMD] CMD-0009 broken")])
        outcomes = self.watcher(gh).poll_once()
        self.assertEqual(outcomes[0].kind, "rejected")
        self.assertIn("close_issue", gh.kinds())
        self.assertIn(aw.LABEL_FAILED, aw.label_names(gh.issues[1]))


class CloseAfterResultTest(WatcherCase):
    def test_close_by_default(self):
        gh = FakeGh([make_issue(body=command_body())])
        self.watcher(gh).poll_once()
        self.assertIn(("close_issue", 1), gh.calls)
        self.assertEqual(gh.issues[1]["state"], "closed")

    def test_close_can_be_switched_off(self):
        self.cfg = config(self.tmp, close_after_result=False)
        gh = FakeGh([make_issue(body=command_body())])
        self.watcher(gh).poll_once()
        self.assertNotIn("close_issue", gh.kinds())
        self.assertEqual(aw.label_names(gh.issues[1]), {aw.LABEL_DONE})

    def test_close_is_skipped_in_dry_run(self):
        gh = FakeGh([make_issue(body=command_body())])
        self.watcher(gh, dry_run=True).poll_once()
        self.assertNotIn("close_issue", gh.kinds())

    def test_failed_close_does_not_break_the_round(self):
        gh = FakeGh([make_issue(body=command_body())])

        def boom(number):
            raise RuntimeError("gh api failed: Resource not accessible by integration")

        gh.close_issue = boom
        outcomes = self.watcher(gh).poll_once()
        self.assertEqual(outcomes[0].kind, "done")
        self.assertIn("close_failed", [event for event, _ in self.log])


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

    def test_allow_limits_are_honoured(self):
        cfg = aw.Config.from_dict({
            "repo": "o/n", "workdir": "/tmp", "allowed_logins": ["x"],
            "allow": {"log_max_lines": 5, "git_log_max": 7,
                      "grep_max_matches": 11, "grep_max_files": 13},
        })
        self.assertEqual((cfg.log_max_lines, cfg.git_log_max, cfg.grep_max_matches,
                          cfg.grep_max_files), (5, 7, 11, 13))

    def test_limits_at_the_root_still_work(self):
        cfg = aw.Config.from_dict({
            "repo": "o/n", "workdir": "/tmp", "allowed_logins": ["x"],
            "log_max_lines": 9, "allow": {},
        })
        self.assertEqual(cfg.log_max_lines, 9)

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


class OnceLockTest(unittest.TestCase):
    """--once takes the same lock as the service: no second execution."""

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)
        (self.tmp / "logs").mkdir()
        self.cfg_path = self.tmp / "w.json"
        self.cfg_path.write_text(json.dumps({
            "repo": "owner/name", "workdir": str(self.tmp), "allowed_logins": ["owner"],
        }), encoding="utf-8")
        self.gh = FakeGh([make_issue(body=command_body(), labels=())])
        self.patch = mock.patch.object(aw, "GhClient", return_value=self.gh)
        self.patch.start()
        self.addCleanup(self.patch.stop)

    def run_main(self, argv):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = aw.main(argv)
        return code, out.getvalue(), err.getvalue()

    def test_once_is_refused_while_the_lock_is_held(self):
        with aw.SingleInstance(self.tmp / ".arena" / "watcher.lock"):
            code, _, err = self.run_main(["--config", str(self.cfg_path), "--once"])
        self.assertEqual(code, 1)
        self.assertIn("another watcher holds", err)
        self.assertEqual(self.gh.calls, [])  # nothing claimed, nothing executed

    def test_once_runs_when_the_lock_is_free(self):
        code, _, _ = self.run_main(["--config", str(self.cfg_path), "--once"])
        self.assertEqual(code, 0)
        self.assertIn("comment", self.gh.kinds())

    def test_dry_run_does_not_need_the_lock(self):
        with aw.SingleInstance(self.tmp / ".arena" / "watcher.lock"):
            code, out, _ = self.run_main(["--config", str(self.cfg_path), "--once", "--dry-run"])
        self.assertIn("dry-run: CMD-0001", out)
        self.assertEqual(code, 0)
        self.assertEqual(self.gh.calls, [])

    def test_stale_pid_in_the_lock_file_does_not_break_locking(self):
        # Windows locks at the current offset; "a+" leaves it after the stale
        # pid, so the next unlock would hit a different byte and fail.
        lock = self.tmp / ".arena" / "watcher.lock"
        lock.parent.mkdir(parents=True, exist_ok=True)
        lock.write_text("999999", encoding="utf-8")
        with aw.SingleInstance(lock):
            pass
        # The pid replaces the stale bytes, and the lock still covers byte 0,
        # so the next acquire is refused while the handle is held and works
        # after release (Windows cannot reopen a locked file, hence the order).
        self.assertEqual(lock.read_text(encoding="utf-8"), str(os.getpid()))
        with aw.SingleInstance(lock):
            pass

    def test_second_lock_in_the_same_process_is_refused(self):
        # Windows locks are per-process: the second handle gets past locking()
        # and would fail later, so the guard has to be explicit.
        lock = self.tmp / ".arena" / "watcher.lock"
        with aw.SingleInstance(lock):
            with self.assertRaises(aw.ConfigError):
                aw.SingleInstance(lock).__enter__()
        with aw.SingleInstance(lock):  # released properly after the refusal
            pass


if __name__ == "__main__":
    unittest.main()
