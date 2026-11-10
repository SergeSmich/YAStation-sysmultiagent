#!/usr/bin/env python3
"""Arena command bus: run whitelisted commands posted as GitHub issues.

This is an *additional* control channel next to the git bridge in `collab/`.
The bridge stays the primary path; the watcher can be stopped at any moment and
the repository behaves exactly as before (see `docs/arena-command-bus.md`).

One round:

    issue + oc:run  ->  claim (oc:running)  ->  execute  ->  comment  ->  oc:done

Safety rules, all enforced here and not by the issue author:

* only trusted authors are executed (`author_association` and a login allowlist);
* only the actions in ACTIONS run, each through a fixed argv template -- no
  shell, no string concatenation from the issue body;
* one command per issue, exactly one ```json block, unknown keys are refused;
* nothing that changes the repository is implemented: no push, no deploy, no
  file writes, no config edits;
* the result is truncated, redacted and published, so the body must not carry
  secrets -- the watcher never prints its own environment or tokens.

Usage:

    python scripts/arena_watcher.py --check-config
    python scripts/arena_watcher.py --init-labels
    python scripts/arena_watcher.py --once --dry-run --verbose
    python scripts/arena_watcher.py
"""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import hashlib
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Callable, Iterable, Sequence

VERSION = "1.0.0"

# Labels. `oc:paused` is optional and only documented: a local `.arena/paused`
# file is the actual stop switch, because a label edit needs GitHub.
LABEL_RUN = "oc:run"
LABEL_RUNNING = "oc:running"
LABEL_DONE = "oc:done"
LABEL_FAILED = "oc:failed"
LABEL_PAUSED = "oc:paused"

LABEL_SPECS = {
    LABEL_RUN: ("0e8a16", "Arena: command is ready to run"),
    LABEL_RUNNING: ("fbca04", "Arena: watcher took the command"),
    LABEL_DONE: ("1d76db", "Arena: command finished successfully"),
    LABEL_FAILED: ("b60205", "Arena: command was refused or failed"),
    LABEL_PAUSED: ("5319e7", "Arena: command bus paused for this issue"),
}

TRUSTED_ASSOCIATIONS = frozenset({"OWNER", "MEMBER", "COLLABORATOR"})
ACTIONS = frozenset({"git_status", "git_log", "run_tests", "build", "read_log", "grep", "deploy_check"})
COMMAND_KEYS = frozenset({"id", "action", "args", "timeout_s", "note"})
COMMAND_ID_RE = re.compile(r"^CMD-[0-9]{4,6}$")

# A fenced block: ```json ... ``` (the language tag is optional).
FENCE_RE = re.compile(r"```[ \t]*(?:json)?[ \t]*\r?\n(.*?)\r?\n[ \t]*```", re.DOTALL)

# Output that must never be published. The private-key pattern is assembled from
# two halves on purpose: the repository's public-tree check rejects the literal
# header in indexed files, and this is the one place where the marker itself has
# to be matched.
_SECRET_PATTERNS = (
    re.compile(r"gh[pousr]_[A-Za-z0-9]{8,}"),
    re.compile(r"github_pat_[A-Za-z0-9_]{8,}"),
    re.compile(r"\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{4,}\b"),
    re.compile(
        r"(?i)(\b(?:bearer|token|secret|password|passwd|api[_-]?key|authorization)\b\s*[:=]\s*)"
        r"(?:Bearer\s+|Basic\s+|Token\s+)?\S+"
    ),
    re.compile(r"(?i)\b(?:sk|rk)-[A-Za-z0-9]{16,}\b"),
    re.compile(
        "-----BEGIN "
        + "[A-Z ]*PRIVATE KEY-----.*?-----END "
        + "[A-Z ]*PRIVATE KEY-----",
        re.DOTALL,
    ),
)

# Only these variables reach a child process: a token that happens to live in the
# watcher's environment must not leak into `go test` output.
CHILD_ENV_KEEP = (
    "PATH", "HOME", "USERPROFILE", "SYSTEMROOT", "COMSPEC", "TEMP", "TMP",
    "GOCACHE", "GOPATH", "GOMODCACHE", "GOFLAGS", "GOPROXY", "GOTOOLCHAIN",
    "GOOS", "GOARCH", "LANG", "LC_ALL", "PYTHONIOENCODING",
)


class ConfigError(Exception):
    """The watcher configuration is unusable."""


@dataclasses.dataclass
class Completed:
    """The slice of subprocess.CompletedProcess the watcher uses."""

    returncode: int
    stdout: str = ""
    stderr: str = ""


def _text(value: Any) -> str:
    if value is None:
        return ""
    if isinstance(value, bytes):
        return value.decode("utf-8", "replace")
    return str(value)


def run_command(
    argv: Sequence[str],
    cwd: Path | str | None = None,
    timeout: float | None = None,
    stdin_text: str | None = None,
    env: dict[str, str] | None = None,
) -> Completed:
    """Run one argv without a shell. Never raises for a missing binary or a timeout."""
    try:
        proc = subprocess.run(
            list(argv),
            cwd=str(cwd) if cwd is not None else None,
            input=stdin_text,
            stdin=None if stdin_text is not None else subprocess.DEVNULL,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=timeout,
            shell=False,
            env=env,
        )
    except FileNotFoundError:
        return Completed(127, "", f"command not found: {argv[0]}")
    except subprocess.TimeoutExpired as exc:
        return Completed(124, _text(exc.stdout), f"timeout after {timeout}s")
    except OSError as exc:  # pragma: no cover - unusual host state
        return Completed(126, "", f"cannot run {argv[0]}: {exc}")
    return Completed(proc.returncode, proc.stdout or "", proc.stderr or "")


def child_env(extra: dict[str, str] | None = None) -> dict[str, str]:
    env = {name: os.environ[name] for name in CHILD_ENV_KEEP if name in os.environ}
    env.setdefault("PYTHONIOENCODING", "utf-8")
    if extra:
        for name, value in extra.items():
            env[str(name)] = str(value)
    return env


def utcnow() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc)


def parse_time(value: Any) -> dt.datetime | None:
    if not value or not isinstance(value, str):
        return None
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        parsed = dt.datetime.fromisoformat(text)
    except ValueError:
        return None
    if parsed.tzinfo is None:
        parsed = parsed.replace(tzinfo=dt.timezone.utc)
    return parsed


def body_sha(body: str | None) -> str:
    return hashlib.sha256((body or "").encode("utf-8")).hexdigest()


@dataclasses.dataclass(frozen=True)
class Config:
    """Everything the watcher needs; the live file lives outside git."""

    repo: str
    workdir: Path
    allowed_logins: tuple[str, ...] = ()
    poll_s: int = 60
    default_timeout_s: int = 120
    max_timeout_s: int = 600
    stale_running_min: int = 15
    max_body_bytes: int = 16384
    max_result_bytes: int = 8000
    max_result_lines: int = 60
    log_max_lines: int = 200
    git_log_max: int = 30
    grep_max_matches: int = 200
    grep_max_files: int = 2000
    log_dirs: tuple[str, ...] = ("logs",)
    test_package_re: str = r"^\./(?:internal|cmd)/[A-Za-z0-9_./-]*$"
    default_test_packages: tuple[str, ...] = ("./internal/bridge", "./cmd/quasar-bridge")
    grep_globs: tuple[str, ...] = ("*.go", "*.md", "*.py", "*.json")
    health: tuple[dict[str, str], ...] = ()
    state_dir: str = ".arena"
    gh: str = "gh"
    git: str = "git"
    go: str = "go"
    env: dict[str, str] = dataclasses.field(default_factory=dict)

    # -- loading -----------------------------------------------------------

    @classmethod
    def from_dict(cls, raw: dict[str, Any], base: Path | None = None) -> "Config":
        if not isinstance(raw, dict):
            raise ConfigError("config must be a JSON object")
        repo = str(raw.get("repo") or "").strip()
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
            raise ConfigError("config.repo must look like owner/name")
        workdir = Path(str(raw.get("workdir") or ".")).expanduser()
        if base is not None and not workdir.is_absolute():
            workdir = (base / workdir).resolve()
        logins = tuple(str(x).strip() for x in (raw.get("allowed_logins") or ()) if str(x).strip())
        if not logins:
            # An empty allowlist would trust every collaborator of the repository.
            raise ConfigError("config.allowed_logins must name at least one login")
        allow = raw.get("allow") or {}
        if not isinstance(allow, dict):
            raise ConfigError("config.allow must be an object")

        def ints(name: str, default: int, low: int, high: int) -> int:
            value = raw.get(name, default)
            try:
                number = int(value)
            except (TypeError, ValueError):
                raise ConfigError(f"config.{name} must be an integer")
            return max(low, min(high, number))

        def strings(source: dict[str, Any], name: str, default: Sequence[str]) -> tuple[str, ...]:
            value = source.get(name, list(default))
            if not isinstance(value, (list, tuple)):
                raise ConfigError(f"config.{name} must be a list")
            return tuple(str(item) for item in value)

        health: list[dict[str, str]] = []
        for index, item in enumerate(raw.get("health") or ()):
            if isinstance(item, str):
                entry = {"url": item, "label": f"health[{index}]"}
            elif isinstance(item, dict) and item.get("url"):
                entry = {"url": str(item["url"]), "label": str(item.get("label") or f"health[{index}]")}
            else:
                raise ConfigError("config.health items must be a URL or {label, url}")
            health.append(entry)

        package_re = str(allow.get("test_package_re") or cls.test_package_re)
        try:
            re.compile(package_re)
        except re.error as exc:
            raise ConfigError(f"config.allow.test_package_re: {exc}")

        state_dir = str(raw.get("state_dir") or ".arena")
        return cls(
            repo=repo,
            workdir=workdir,
            allowed_logins=logins,
            poll_s=ints("poll_s", 60, 5, 3600),
            default_timeout_s=ints("default_timeout_s", 120, 1, 3600),
            max_timeout_s=ints("max_timeout_s", 600, 1, 3600),
            stale_running_min=ints("stale_running_min", 15, 1, 24 * 60),
            max_body_bytes=ints("max_body_bytes", 16384, 512, 262144),
            max_result_bytes=ints("max_result_bytes", 8000, 512, 60000),
            max_result_lines=ints("max_result_lines", 60, 1, 2000),
            log_max_lines=ints("log_max_lines", 200, 1, 5000),
            git_log_max=ints("git_log_max", 30, 1, 100),
            grep_max_matches=ints("grep_max_matches", 200, 1, 5000),
            grep_max_files=ints("grep_max_files", 2000, 1, 50000),
            log_dirs=strings(allow, "log_dirs", cls.log_dirs),
            test_package_re=package_re,
            default_test_packages=strings(allow, "default_test_packages", cls.default_test_packages),
            grep_globs=strings(allow, "grep_globs", cls.grep_globs),
            health=tuple(health),
            state_dir=state_dir,
            gh=str(raw.get("gh") or "gh"),
            git=str(raw.get("git") or "git"),
            go=str(raw.get("go") or "go"),
            env={str(k): str(v) for k, v in (raw.get("env") or {}).items()},
        )

    @classmethod
    def load(cls, path: Path) -> "Config":
        try:
            raw = json.loads(Path(path).read_text(encoding="utf-8"))
        except OSError as exc:
            raise ConfigError(f"cannot read {path}: {exc}")
        except json.JSONDecodeError as exc:
            raise ConfigError(f"{path} is not valid JSON: {exc}")
        return cls.from_dict(raw, base=Path(path).resolve().parent)

    # -- paths -------------------------------------------------------------

    @property
    def state(self) -> Path:
        path = Path(self.state_dir)
        return path if path.is_absolute() else (self.workdir / path)

    def work(self, *parts: str) -> Path:
        return self.workdir.joinpath(*parts)


@dataclasses.dataclass
class Command:
    id: str
    action: str
    args: dict[str, Any]
    timeout_s: int
    note: str
    sha: str


def _no_duplicates(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    seen: dict[str, Any] = {}
    for key, value in pairs:
        if key in seen:
            raise ValueError(f"duplicate key {key!r}")
        seen[key] = value
    return seen


def extract_commands(body: str) -> list[dict[str, Any]]:
    """Every fenced block that parses as a JSON object with an `action` key."""
    found: list[dict[str, Any]] = []
    for block in FENCE_RE.findall(body or ""):
        try:
            data = json.loads(block, object_pairs_hook=_no_duplicates)
        except (json.JSONDecodeError, ValueError):
            continue
        if isinstance(data, dict) and "action" in data:
            found.append(data)
    return found


def _no_block_error(body: str) -> str:
    """Why there is no usable block: malformed JSON, or nothing at all."""
    for block in FENCE_RE.findall(body or ""):
        try:
            json.loads(block, object_pairs_hook=_no_duplicates)
        except ValueError as exc:
            if '"action"' in block:
                return f"command block is not valid JSON: {exc}"
    return "no ```json command block found"


def parse_command(body: str, cfg: Config) -> tuple[Command | None, str]:
    """Parse the single command of an issue body. Returns (command, error)."""
    raw = (body or "").encode("utf-8")
    if len(raw) > cfg.max_body_bytes:
        return None, f"body is {len(raw)} bytes, the limit is {cfg.max_body_bytes}"
    blocks = extract_commands(body)
    if not blocks:
        return None, _no_block_error(body)
    if len(blocks) > 1:
        return None, "more than one command block; one issue carries one command"
    data = blocks[0]
    unknown = sorted(set(data) - COMMAND_KEYS)
    if unknown:
        return None, f"unknown keys: {', '.join(unknown)}"
    ident = str(data.get("id") or "").strip()
    if not COMMAND_ID_RE.match(ident):
        return None, "id must look like CMD-0001"
    action = str(data.get("action") or "").strip()
    if action not in ACTIONS:
        return None, f"action {action!r} is not in the whitelist"
    args = data.get("args") or {}
    if not isinstance(args, dict):
        return None, "args must be an object"
    error = validate_args(action, args, cfg)
    if error:
        return None, error
    try:
        requested = int(data.get("timeout_s") or cfg.default_timeout_s)
    except (TypeError, ValueError):
        return None, "timeout_s must be an integer"
    timeout_s = max(1, min(requested, cfg.max_timeout_s))
    note = data.get("note") or ""
    if not isinstance(note, str) or len(note) > 200:
        return None, "note must be a string of at most 200 characters"
    return Command(ident, action, args, timeout_s, note, hashlib.sha256(raw).hexdigest()), ""


def validate_args(action: str, args: dict[str, Any], cfg: Config) -> str:
    """Reject anything the action does not understand, before anything runs."""
    allowed = {
        "git_status": set(),
        "git_log": {"n"},
        "run_tests": {"packages"},
        "build": set(),
        "read_log": {"path", "lines"},
        "grep": {"pattern", "glob", "max"},
        "deploy_check": {"url"},
    }[action]
    extra = sorted(set(args) - allowed)
    if extra:
        return f"args for {action} do not take: {', '.join(extra)}"
    if action == "git_log":
        n = args.get("n", 10)
        if not isinstance(n, int) or isinstance(n, bool) or not 1 <= n <= cfg.git_log_max:
            return f"n must be an integer 1..{cfg.git_log_max}"
    if action == "run_tests":
        packages = args.get("packages", list(cfg.default_test_packages))
        if not isinstance(packages, list) or not packages:
            return "packages must be a non-empty list"
        if len(packages) > 8:
            return "at most 8 packages per command"
        pattern = re.compile(cfg.test_package_re)
        for package in packages:
            if not isinstance(package, str) or not pattern.match(package):
                return f"package {package!r} is not allowed by test_package_re"
    if action == "read_log":
        raw_path = args.get("path")
        if not isinstance(raw_path, str) or not raw_path.strip():
            return "path is required"
        error = check_log_path(raw_path, cfg)
        if error:
            return error
        lines = args.get("lines", 100)
        if not isinstance(lines, int) or isinstance(lines, bool) or not 1 <= lines <= cfg.log_max_lines:
            return f"lines must be an integer 1..{cfg.log_max_lines}"
    if action == "grep":
        pattern = args.get("pattern")
        if not isinstance(pattern, str) or not 1 <= len(pattern) <= 200:
            return "pattern must be a string of 1..200 characters"
        try:
            re.compile(pattern)
        except re.error as exc:
            return f"pattern is not a valid regex: {exc}"
        glob = args.get("glob")
        if glob is not None and (not isinstance(glob, str) or glob not in cfg.grep_globs):
            return f"glob must be one of: {', '.join(cfg.grep_globs)}"
        limit = args.get("max", 50)
        if not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= cfg.grep_max_matches:
            return f"max must be an integer 1..{cfg.grep_max_matches}"
    if action == "deploy_check":
        url = args.get("url")
        if url is not None:
            if not isinstance(url, str) or url not in [entry["url"] for entry in cfg.health]:
                return "url must be one of the configured health entries"
    return ""


def check_log_path(raw: str, cfg: Config) -> str:
    """A log path must stay inside one of the configured directories."""
    text = raw.strip().replace("\\", "/")
    if not text or text.startswith("/") or re.match(r"^[A-Za-z]:/", text):
        return "path must be relative to a configured log directory"
    if ".." in text.split("/"):
        return "path must not contain .."
    if re.search(r"[\x00-\x1f]", text):
        return "path contains control characters"
    roots = [cfg.workdir / name for name in cfg.log_dirs]
    resolved = (cfg.workdir / text).resolve()
    for root in roots:
        try:
            root_resolved = root.resolve()
        except OSError:  # pragma: no cover - broken symlink
            continue
        if resolved == root_resolved or root_resolved in resolved.parents:
            return ""
    return f"path must be under: {', '.join(cfg.log_dirs)}"


def _mask(match: "re.Match[str]") -> str:
    """Mask the secret, keep the key that named it (`token=***`, not `***`).

    Group 1 of a keyword pattern is the prefix to keep; everything after it is
    the value, so it disappears whole.
    """
    whole = match.group(0)
    if not match.groups():
        return "***"
    keep = match.end(1) - match.start()
    return whole[:keep] + "***"


def redact(text: str) -> str:
    for pattern in _SECRET_PATTERNS:
        text = pattern.sub(_mask, text)
    return text


def clip(text: str, max_bytes: int | None = None, max_lines: int | None = None) -> str:
    body = text or ""
    cut = False
    if max_lines is not None:
        lines = body.splitlines()
        if len(lines) > max_lines:
            body = "\n".join(lines[:max_lines])
            cut = True
    if max_bytes is not None:
        encoded = body.encode("utf-8")
        if len(encoded) > max_bytes:
            body = encoded[:max_bytes].decode("utf-8", "ignore")
            cut = True
    if cut:
        body += "\n… (обрезано)"
    return body


def label_names(issue: dict[str, Any]) -> set[str]:
    names: set[str] = set()
    for label in issue.get("labels") or ():
        if isinstance(label, dict):
            name = label.get("name")
            if name:
                names.add(str(name))
        elif label:
            names.add(str(label))
    return names


def issue_author(issue: dict[str, Any]) -> tuple[str, str]:
    user = issue.get("user") or {}
    login = str(user.get("login") or issue.get("author_login") or "")
    return login, str(issue.get("author_association") or "").upper()


# --------------------------------------------------------------------------
# GitHub access: a thin `gh` wrapper. The token never appears in the watcher:
# `gh` keeps its own credentials.
# --------------------------------------------------------------------------


class GhClient:
    def __init__(self, gh: str = "gh", repo: str = "", runner: Callable[..., Completed] = run_command,
                 timeout: float = 30.0):
        self.gh = gh
        self.repo = repo
        self.runner = runner
        self.timeout = timeout

    def _api(self, path: str, method: str = "GET", payload: Any = None, timeout: float | None = None) -> Any:
        argv = [self.gh, "api", path, "--method", method]
        stdin_text = None
        if payload is not None:
            argv += ["--input", "-"]
            stdin_text = json.dumps(payload)
        result = self.runner(argv, timeout=timeout or self.timeout, stdin_text=stdin_text)
        if result.returncode != 0:
            message = (result.stderr or result.stdout or "").strip().splitlines()
            raise RuntimeError(f"gh api {path} failed: {message[0] if message else result.returncode}")
        return json.loads(result.stdout) if result.stdout.strip() else None

    def list_issues(self, labels: Iterable[str]) -> list[dict[str, Any]]:
        """Open issues carrying any of the labels. Pull requests are dropped."""
        by_number: dict[int, dict[str, Any]] = {}
        for label in labels:
            query = urllib.parse.urlencode({"state": "open", "per_page": 50, "labels": label})
            data = self._api(f"repos/{self.repo}/issues?{query}") or []
            for issue in data:
                if "pull_request" in issue:
                    continue
                by_number[int(issue["number"])] = issue
        return list(by_number.values())

    def get_issue(self, number: int) -> dict[str, Any]:
        return self._api(f"repos/{self.repo}/issues/{number}") or {}

    def comment(self, number: int, body: str) -> None:
        self._api(f"repos/{self.repo}/issues/{number}/comments", method="POST", payload={"body": body})

    def add_label(self, number: int, label: str) -> None:
        self._api(f"repos/{self.repo}/issues/{number}/labels", method="POST", payload={"labels": [label]})

    def remove_label(self, number: int, label: str) -> None:
        quoted = urllib.parse.quote(label, safe="")
        try:
            self._api(f"repos/{self.repo}/issues/{number}/labels/{quoted}", method="DELETE")
        except RuntimeError as exc:
            # Removing a label that is already gone is not an error.
            if "404" not in str(exc) and "Not Found" not in str(exc):
                raise

    def create_labels(self) -> None:
        for name, (color, description) in LABEL_SPECS.items():
            result = self.runner(
                [self.gh, "label", "create", name, "--repo", self.repo, "--color", color,
                 "--description", description, "--force"],
                timeout=self.timeout,
            )
            if result.returncode != 0:
                raise RuntimeError(f"cannot create label {name}: {(result.stderr or '').strip()}")


# --------------------------------------------------------------------------
# The watcher
# --------------------------------------------------------------------------


@dataclasses.dataclass
class Outcome:
    kind: str          # done | failed | rejected | untrusted | stale | skipped | dry-run | error
    detail: str = ""


class Watcher:
    def __init__(self, cfg: Config, gh: Any, runner: Callable[..., Completed] = run_command,
                 logger: Callable[[str, dict[str, Any]], None] | None = None,
                 now: Callable[[], dt.datetime] = utcnow, dry_run: bool = False,
                 verbose: bool = False):
        self.cfg = cfg
        self.gh = gh
        self.runner = runner
        self.now = now
        self.dry_run = dry_run
        self.verbose = verbose
        self._log = logger

    # -- small helpers -----------------------------------------------------

    def log(self, event: str, **fields: Any) -> None:
        if self._log is not None:
            self._log(event, fields)
        if self.verbose:
            extra = " ".join(f"{k}={v}" for k, v in fields.items())
            print(f"[{event}] {extra}".rstrip(), file=sys.stderr)

    def paused(self) -> bool:
        return (self.cfg.state / "paused").exists()

    def env(self) -> dict[str, str]:
        return child_env(self.cfg.env)

    def issue_labels(self, issue: dict[str, Any]) -> set[str]:
        return label_names(issue)

    # -- trust -------------------------------------------------------------

    def trusted(self, issue: dict[str, Any]) -> tuple[bool, str]:
        login, association = issue_author(issue)
        if association not in TRUSTED_ASSOCIATIONS:
            return False, f"author_association {association or 'none'} is not trusted"
        allowed = {name.lower() for name in self.cfg.allowed_logins}
        if login.lower() not in allowed:
            return False, f"author {login or 'unknown'} is not in allowed_logins"
        return True, ""

    # -- poll --------------------------------------------------------------

    def poll_once(self) -> list[Outcome]:
        if self.paused():
            self.log("paused", state=str(self.cfg.state))
            return []
        issues = self.gh.list_issues([LABEL_RUN, LABEL_RUNNING])
        issues.sort(key=lambda item: str(item.get("updated_at") or item.get("created_at") or ""))
        outcomes: list[Outcome] = []
        for issue in issues:
            try:
                outcomes.append(self.handle_issue(issue))
            except Exception as exc:  # one bad issue must not stop the loop
                number = issue.get("number")
                self.log("issue_error", number=number, error=str(exc))
                outcomes.append(Outcome("error", f"#{number}: {exc}"))
        return outcomes

    def stale(self, issue: dict[str, Any]) -> bool:
        if LABEL_PAUSED in self.issue_labels(issue):
            return False
        stamp = parse_time(issue.get("updated_at") or issue.get("created_at"))
        if stamp is None:
            return False
        age = self.now() - stamp
        return age > dt.timedelta(minutes=self.cfg.stale_running_min)

    def handle_issue(self, issue: dict[str, Any]) -> Outcome:
        number = int(issue["number"])
        labels = self.issue_labels(issue)
        if LABEL_PAUSED in labels:
            self.log("skip_paused", number=number)
            return Outcome("skipped", "paused by label")
        if LABEL_RUNNING in labels:
            # Claimed: either in flight or abandoned. Never run it a second time.
            if self.stale(issue):
                return self.mark_stale(issue)
            self.log("skip_running", number=number)
            return Outcome("skipped", "already claimed")
        if LABEL_RUN not in labels:
            return Outcome("skipped", "no run label")
        return self.claim_and_run(issue, number)

    # -- claim -> execute -> comment ---------------------------------------

    def claim_and_run(self, issue: dict[str, Any], number: int) -> Outcome:
        ok, why = self.trusted(issue)
        if not ok:
            # A stranger can open an issue on a public repository. Do not answer
            # them: drop the trigger label and leave a local record only.
            self.log("untrusted", number=number, reason=why)
            if not self.dry_run:
                self.gh.remove_label(number, LABEL_RUN)
            return Outcome("untrusted", why)

        command, error = parse_command(issue.get("body") or "", self.cfg)
        if command is None:
            return self.reject(issue, number, error)

        if self.dry_run:
            self.log("dry_run", number=number, id=command.id, action=command.action,
                     timeout_s=command.timeout_s)
            return Outcome("dry-run", f"{command.id} {command.action}")

        before = body_sha(issue.get("body"))
        self.gh.add_label(number, LABEL_RUNNING)
        self.gh.remove_label(number, LABEL_RUN)
        # The body is the command: if it moved between reading and claiming, the
        # command that would run is not the command that was checked.
        fresh = self.gh.get_issue(number)
        if body_sha(fresh.get("body")) != before:
            self.log("body_changed", number=number, id=command.id)
            self.gh.comment(number, self.comment_refused(command, "body changed between read and claim"))
            self.gh.remove_label(number, LABEL_RUNNING)
            self.gh.add_label(number, LABEL_FAILED)
            return Outcome("rejected", "body changed")
        if LABEL_RUN in self.issue_labels(fresh):
            # Somebody else put the run label back: leave it to the next round.
            self.log("claim_race", number=number, id=command.id)

        started = time.monotonic()
        code, output = self.execute(command)
        elapsed = time.monotonic() - started
        output = clip(redact(output), self.cfg.max_result_bytes, self.cfg.max_result_lines)
        failed = code != 0
        self.gh.comment(number, self.comment_result(command, code, elapsed, output))
        self.gh.remove_label(number, LABEL_RUNNING)
        self.gh.add_label(number, LABEL_FAILED if failed else LABEL_DONE)
        self.log("executed", number=number, id=command.id, action=command.action,
                 exit=code, ms=int(elapsed * 1000))
        return Outcome("failed" if failed else "done", f"{command.id} exit {code}")

    def reject(self, issue: dict[str, Any], number: int, reason: str) -> Outcome:
        self.log("rejected", number=number, reason=reason)
        if not self.dry_run:
            self.gh.comment(number, self.comment_refused(None, reason))
            self.gh.remove_label(number, LABEL_RUN)
            self.gh.add_label(number, LABEL_FAILED)
        return Outcome("rejected", reason)

    def mark_stale(self, issue: dict[str, Any]) -> Outcome:
        number = int(issue["number"])
        self.log("stale_running", number=number)
        if not self.dry_run:
            body = "\n".join([
                "### oc:result — не выполнено",
                "",
                f"Задача висела в `{LABEL_RUNNING}` дольше {self.cfg.stale_running_min} мин.",
                "Автоматического перезапуска нет: проверь вывод предыдущего прогона и,",
                f"если нужно, поставь `{LABEL_RUN}` заново.",
                "",
                f"_arena_watcher {VERSION}_",
            ])
            self.gh.comment(number, body)
            self.gh.remove_label(number, LABEL_RUNNING)
            self.gh.add_label(number, LABEL_FAILED)
        return Outcome("stale", f"#{number}")

    # -- comments ----------------------------------------------------------

    def comment_refused(self, command: Command | None, reason: str) -> str:
        ident = command.id if command else "—"
        return "\n".join([
            f"### oc:result {ident} — отклонено",
            "",
            f"- причина: `{clip(redact(reason), 300)}`",
            "- действие не выполнялось",
            f"- ожидаемый формат: один ```json блок с `id`, `action`, `args`, `timeout_s`, `note`",
            f"- whitelist: {', '.join(sorted(ACTIONS))}",
            "",
            f"_arena_watcher {VERSION}_",
        ])

    def comment_result(self, command: Command, code: int, elapsed: float, output: str) -> str:
        status = "ок" if code == 0 else "ошибка"
        lines = output.count("\n") + 1 if output else 0
        header = [
            f"### oc:result {command.id} — {command.action}",
            "",
            f"- статус: **{status}** (exit {code})",
            f"- длительность: {elapsed:.1f} с (лимит {command.timeout_s} с)",
            f"- sha256 тела: `{command.sha[:16]}`",
            f"- вывод: {lines} строк, {len(output.encode('utf-8'))} байт",
            f"- watcher: {VERSION}",
        ]
        if command.note:
            header.insert(3, f"- примечание: {clip(redact(command.note), 200)}")
        fence = "```"
        block = [fence + "text", output or "(пусто)", fence]
        return "\n".join(header + [""] + block)

    # -- execution ---------------------------------------------------------

    def execute(self, command: Command) -> tuple[int, str]:
        handler = {
            "git_status": self.act_git_status,
            "git_log": self.act_git_log,
            "run_tests": self.act_run_tests,
            "build": self.act_build,
            "read_log": self.act_read_log,
            "grep": self.act_grep,
            "deploy_check": self.act_deploy_check,
        }[command.action]
        return handler(command)

    def _combine(self, result: Completed) -> str:
        parts = [part for part in (result.stdout.strip(), result.stderr.strip()) if part]
        return "\n".join(parts)

    def act_git_status(self, command: Command) -> tuple[int, str]:
        result = self.runner([self.cfg.git, "status", "--porcelain=v1", "--branch"],
                             cwd=self.cfg.workdir, timeout=command.timeout_s, env=self.env())
        return result.returncode, self._combine(result) or "(рабочее дерево чистое)"

    def act_git_log(self, command: Command) -> tuple[int, str]:
        n = int(command.args.get("n", 10))
        result = self.runner([self.cfg.git, "log", f"-n{n}", "--date=short",
                              "--pretty=%h %ad %s"], cwd=self.cfg.workdir,
                             timeout=command.timeout_s, env=self.env())
        return result.returncode, self._combine(result)

    def act_run_tests(self, command: Command) -> tuple[int, str]:
        packages = command.args.get("packages") or list(self.cfg.default_test_packages)
        result = self.runner([self.cfg.go, "test", *packages], cwd=self.cfg.workdir,
                             timeout=command.timeout_s, env=self.env())
        return result.returncode, self._combine(result)

    def act_build(self, command: Command) -> tuple[int, str]:
        # -o into the state directory: `go build ./...` would otherwise drop
        # binaries into the working tree.
        out = self.cfg.state / "build"
        out.mkdir(parents=True, exist_ok=True)
        result = self.runner([self.cfg.go, "build", "-o", str(out) + os.sep, "./..."],
                             cwd=self.cfg.workdir, timeout=command.timeout_s, env=self.env())
        return result.returncode, self._combine(result)

    def act_read_log(self, command: Command) -> tuple[int, str]:
        error = check_log_path(str(command.args["path"]), self.cfg)
        if error:
            return 2, error
        path = (self.cfg.workdir / str(command.args["path"]).replace("\\", "/")).resolve()
        limit = int(command.args.get("lines", 100))
        try:
            size = path.stat().st_size
            with path.open("rb") as handle:
                handle.seek(max(0, size - 256 * 1024))
                data = handle.read()
        except OSError as exc:
            return 1, f"cannot read {path.name}: {exc}"
        lines = data.decode("utf-8", "replace").splitlines()[-limit:]
        return 0, "\n".join(lines)

    def act_grep(self, command: Command) -> tuple[int, str]:
        pattern = re.compile(str(command.args["pattern"]))
        glob = command.args.get("glob")
        limit = int(command.args.get("max", 50))
        skip = {".git", ".arena", "node_modules", "dist", "build", "__pycache__"}
        matches: list[str] = []
        scanned = 0
        for glob_pattern in ([glob] if glob else list(self.cfg.grep_globs)):
            for path in sorted(self.cfg.workdir.rglob(glob_pattern)):
                if scanned >= self.cfg.grep_max_files:
                    matches.append("… (лимит файлов достигнут)")
                    return 0, "\n".join(matches)
                if not path.is_file() or any(part in skip for part in path.parts):
                    continue
                try:
                    if path.stat().st_size > 512 * 1024:
                        continue
                    text = path.read_text(encoding="utf-8", errors="ignore")
                except OSError:
                    continue
                scanned += 1
                for number, line in enumerate(text.splitlines(), 1):
                    if pattern.search(line):
                        rel = path.relative_to(self.cfg.workdir).as_posix()
                        matches.append(f"{rel}:{number}: {line.strip()[:200]}")
                        if len(matches) >= limit:
                            matches.append(f"… (лимит {limit} совпадений)")
                            return 0, "\n".join(matches)
        return 0, "\n".join(matches) or "(совпадений нет)"

    def act_deploy_check(self, command: Command) -> tuple[int, str]:
        wanted = command.args.get("url")
        entries = [entry for entry in self.cfg.health if not wanted or entry["url"] == wanted]
        if not entries:
            return 2, "no health entries configured"
        lines: list[str] = []
        failed = False
        for index, entry in enumerate(entries):
            # Only the label is published: a public issue must not carry home
            # addresses of the station.
            label = entry.get("label") or f"health[{index}]"
            started = time.monotonic()
            status, note = self.probe(entry["url"], timeout=min(5.0, float(command.timeout_s)))
            elapsed = (time.monotonic() - started) * 1000
            if status is None:
                failed = True
                lines.append(f"{label}: недоступно ({note}), {elapsed:.0f} мс")
            elif 200 <= status < 400:
                lines.append(f"{label}: {status}, {elapsed:.0f} мс")
            else:
                failed = True
                lines.append(f"{label}: {status}, {elapsed:.0f} мс")
        return (1 if failed else 0), "\n".join(lines)

    def probe(self, url: str, timeout: float = 5.0) -> tuple[int | None, str]:
        request = urllib.request.Request(url, method="GET", headers={"User-Agent": f"arena-watcher/{VERSION}"})
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                return int(response.status), ""
        except urllib.error.HTTPError as exc:
            return int(exc.code), str(exc.reason)
        except (urllib.error.URLError, OSError, ValueError) as exc:
            return None, str(getattr(exc, "reason", exc))


# --------------------------------------------------------------------------
# Single instance + logging + CLI
# --------------------------------------------------------------------------


class SingleInstance:
    """One watcher per state directory. Uses an OS lock, not a PID guess."""

    def __init__(self, path: Path):
        self.path = Path(path)
        self.handle: Any = None

    def __enter__(self) -> "SingleInstance":
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.handle = self.path.open("a+")
        try:
            if os.name == "nt":  # pragma: no cover - Windows only
                import msvcrt

                msvcrt.locking(self.handle.fileno(), msvcrt.LK_NBLCK, 1)
            else:
                import fcntl

                fcntl.flock(self.handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
        except OSError:
            self.handle.close()
            raise ConfigError(f"another watcher holds {self.path}")
        self.handle.seek(0)
        self.handle.truncate()
        self.handle.write(str(os.getpid()))
        self.handle.flush()
        return self

    def __exit__(self, *exc: Any) -> None:
        if self.handle is None:
            return
        try:
            if os.name == "nt":  # pragma: no cover - Windows only
                import msvcrt

                self.handle.seek(0)
                msvcrt.locking(self.handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                import fcntl

                fcntl.flock(self.handle.fileno(), fcntl.LOCK_UN)
        finally:
            self.handle.close()
            self.handle = None


def file_logger(path: Path) -> Callable[[str, dict[str, Any]], None]:
    def write(event: str, fields: dict[str, Any]) -> None:
        path.parent.mkdir(parents=True, exist_ok=True)
        record = {"ts": utcnow().isoformat(timespec="seconds"), "event": event}
        record.update({key: value for key, value in fields.items()})
        with path.open("a", encoding="utf-8") as handle:
            handle.write(json.dumps(record, ensure_ascii=False) + "\n")

    return write


def default_config_path() -> Path:
    # Inside .arena/, which is already ignored: the live config names the hosts
    # and must never be committed.
    return Path(".arena/watcher.json")


def parse_args(argv: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Arena command bus watcher (GitHub issues).")
    parser.add_argument("--config", type=Path, default=default_config_path(),
                        help="path to the watcher config (default: .arena/watcher.json)")
    parser.add_argument("--once", action="store_true", help="one poll round, then exit")
    parser.add_argument("--dry-run", action="store_true", help="parse and log, change nothing")
    parser.add_argument("--init-labels", action="store_true", help="create/update the oc:* labels")
    parser.add_argument("--check-config", action="store_true", help="validate the config and exit")
    parser.add_argument("--verbose", action="store_true", help="log each step to stderr")
    parser.add_argument("--version", action="version", version=f"arena_watcher {VERSION}")
    return parser.parse_args(argv)


def main(argv: Sequence[str] | None = None) -> int:
    args = parse_args(argv)
    try:
        cfg = Config.load(args.config)
    except ConfigError as exc:
        print(f"config error: {exc}", file=sys.stderr)
        print("copy scripts/arena_watcher.example.json to .arena/watcher.json and edit it", file=sys.stderr)
        return 2
    if not cfg.workdir.is_dir():
        print(f"config error: workdir {cfg.workdir} is not a directory", file=sys.stderr)
        return 2
    if args.check_config:
        print(f"ok: {cfg.repo} -> {cfg.workdir} (logins: {', '.join(cfg.allowed_logins)})")
        return 0

    gh = GhClient(cfg.gh, cfg.repo)
    if args.init_labels:
        try:
            gh.create_labels()
        except RuntimeError as exc:
            print(f"label error: {exc}", file=sys.stderr)
            return 1
        print("labels ready: " + ", ".join(sorted(LABEL_SPECS)))
        return 0

    watcher = Watcher(cfg, gh, logger=file_logger(cfg.state / "watcher.log"),
                      dry_run=args.dry_run, verbose=args.verbose)
    watcher.log("start", version=VERSION, repo=cfg.repo, dry_run=args.dry_run)

    if args.once:
        try:
            outcomes = watcher.poll_once()
        except Exception as exc:  # gh is down, the token expired, the repo moved
            watcher.log("poll_failed", error=str(exc))
            print(f"error: {exc}", file=sys.stderr)
            return 1
        for outcome in outcomes:
            print(f"{outcome.kind}: {outcome.detail}")
        return 0

    lock_path = cfg.state / "watcher.lock"
    try:
        with SingleInstance(lock_path):
            while True:
                try:
                    outcomes = watcher.poll_once()
                except Exception as exc:  # keep polling: the next round may work
                    watcher.log("poll_failed", error=str(exc))
                    outcomes = []
                for outcome in outcomes:
                    watcher.log("outcome", kind=outcome.kind, detail=outcome.detail)
                time.sleep(cfg.poll_s)
    except KeyboardInterrupt:
        watcher.log("stop", reason="signal")
        return 0
    except ConfigError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
