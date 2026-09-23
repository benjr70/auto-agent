#!/usr/bin/env python3
"""auto-agent Dashboard: the read-only status page one Host serves for its own
Daemon (ADR 0006). Carried over from Smart-Smoker-V2's agent-dashboard; the
journal regex parser and the Python copy of the budget gate are gone.

Routes (no POST route exists, so the page cannot change anything):
  GET /            the single page (index.html, beside this file)
  GET /api/status  the JSON seam; its shape is documented in README.md

Inputs, and nothing else:
  - Fire records      <state>/fires/*.json (lib/fire-record.sh): history, the
                      Fire in flight (endedAt null), the bootstrap warning
  - the Gate verdict  `auto-agent usage-sensor`, shown verbatim
  - Daemon state      <state>/daemon-state.json (lib/daemon.sh), parked.json,
                      systemctl; journalctl only for the live tail
  - the queue         `auto-agent work-probe`
  - the repo          `auto-agent show-config` (the Harness config)
  - PRs and Maps      gh, with the harness vocabulary from lib/harness-config.sh
  - the Host env      bind, port, summary toggle (README.md has the table)

Every section degrades on its own: a failing refresh keeps the last good
value flagged stale with the error string, so /api/status never 500s.
"""

import glob
import json
import os
import re
import subprocess
import sys
import tempfile
import threading
import time
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)

# The Host env. The Dashboard's own keys (and their defaults) are documented
# in README.md; the State dir default is lib/host-env.sh's.
BIND = os.environ.get("AUTO_AGENT_DASHBOARD_BIND") or "127.0.0.1"
PORT = int(os.environ.get("AUTO_AGENT_DASHBOARD_PORT") or "8090")
SUMMARY_ENABLED = (os.environ.get("AUTO_AGENT_DASHBOARD_SUMMARY") or "1").strip().lower() not in (
    "0", "false", "no", "off")
SUMMARY_MODEL = os.environ.get("AUTO_AGENT_DASHBOARD_SUMMARY_MODEL") or "haiku"
TARGET = os.environ.get("AUTO_AGENT_TARGET_DIR") or ""
STATE_DIR = os.environ.get("AUTO_AGENT_STATE_DIR") or os.path.join(
    os.environ.get("XDG_STATE_HOME") or os.path.expanduser("~/.local/state"), "auto-agent")
AUTH_MODE = os.environ.get("CLAUDE_AUTH_MODE") or None

# Commands (test seams).
AUTO_AGENT_BIN = os.environ.get("AUTO_AGENT_BIN") or os.path.join(ROOT, "bin", "auto-agent")
GH_BIN = os.environ.get("GH_BIN") or "gh"
CLAUDE_BIN = os.environ.get("CLAUDE_BIN") or "claude"
SYSTEMCTL_BIN = os.environ.get("SYSTEMCTL_BIN") or "systemctl"
JOURNALCTL_BIN = os.environ.get("JOURNALCTL_BIN") or "journalctl"

# The Daemon unit as lib/unit-render.sh names it (its SyslogIdentifier too).
DAEMON_UNIT = "auto-agent-daemon"
FIRES_SHOWN = 10
TAIL_LINES = 30
# A crashed Fire leaves its in-flight record behind; past this age it is
# history, not the current Fire.
IN_FLIGHT_MAX = timedelta(hours=3)


def now_iso():
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def run(cmd, timeout, cwd=None, check=True):
    """stdout of cmd; raises on a non-zero exit when check is set."""
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout, cwd=cwd)
    if check and out.returncode != 0:
        raise RuntimeError(f"{os.path.basename(cmd[0])} exit {out.returncode}: {out.stderr.strip()[:200]}")
    return out


def load_vocab():
    """The harness vocabulary, from lib/harness-config.sh, never re-spelled."""
    script = (
        '. "$1/lib/harness-config.sh" && jq -n -c '
        '--arg map "$HARNESS_LABEL_MAP" --arg afk "$HARNESS_LABEL_AFK" --arg hitl "$HARNESS_LABEL_HITL" '
        '--arg inProgress "$HARNESS_LABEL_IN_PROGRESS" --arg wayfinder "$HARNESS_LABEL_WAYFINDER_PREFIX" '
        '--arg research "$HARNESS_BRANCH_RESEARCH_PREFIX" \'$ARGS.named\''
    )
    return json.loads(run(["bash", "-c", script, "vocab", ROOT], timeout=30).stdout)


VOCAB = load_vocab()


def read_json(path):
    """The file's JSON, or None when it does not exist."""
    try:
        with open(path) as f:
            return json.load(f)
    except FileNotFoundError:
        return None


# --- cache: each section keeps its last good value; a failing refresh serves
# --- the stale value with the error attached instead of breaking the page.
_cache = {}
_cache_locks = {}
_cache_guard = threading.Lock()


def cached(key, ttl, fn):
    with _cache_guard:
        lock = _cache_locks.setdefault(key, threading.Lock())
    with lock:
        entry = _cache.get(key)
        if entry and entry["expires"] > time.monotonic():
            return entry["value"]
        try:
            value = fn()
            value["asOf"] = now_iso()
            value["stale"] = False
            value["error"] = None
        except Exception as e:  # degrade, never 500
            value = dict(entry["value"]) if entry else {}
            value["stale"] = True
            value["error"] = str(e)[:300]
        _cache[key] = {"expires": time.monotonic() + ttl, "value": value}
        return value


# --- Fire records -----------------------------------------------------------

_RESOLVE_LINE_RE = re.compile(r"^resolve:\s+#\d+\s+(\w+)")
_LINE_VALUE_RE = re.compile(r"^[\w-]+:\s+(.*)$")


def _line_value(line):
    """`picked:   #36 title` -> `#36 title`."""
    m = _LINE_VALUE_RE.match((line or "").strip())
    return m.group(1).strip() if m else (line or "").strip()


def fire_badge(rec):
    """What the Fire was, from its record's work block: slice, reconcile,
    deployed, the resolve's ticket type, dry-run, noop, or None (no work)."""
    work = rec.get("work") or {}
    kind = work.get("kind")
    if rec.get("kind") == "noop":
        return "noop"
    if kind == "pick":
        return "slice"
    if kind in ("reconcile", "deployed", "dry-run"):
        return kind
    if kind == "resolve":
        m = _RESOLVE_LINE_RE.match(work.get("line") or "")
        return m.group(1) if m and m.group(1) in ("research", "task") else "resolve"
    return None


def fire_summary(rec):
    """One line for the history row."""
    if rec.get("endedAt") is None:
        return "in flight"
    exit_code = rec.get("exit")
    if rec.get("phase") == "preflight":
        return f"preflight failed — exit {exit_code}"
    work = rec.get("work") or {}
    kind = work.get("kind")
    status = (rec.get("outcome") or {}).get("status")
    if kind in ("pick", "reconcile", "deployed"):
        what = _line_value(work.get("pickedLine") or work.get("line"))
    elif kind == "resolve":
        what = "resolve " + _line_value(work.get("line"))
    elif kind == "dry-run":
        what = work.get("line") or "dry-run"
    elif kind == "none":
        what = "no work — queue empty"
    else:
        what = ""
    if status == "EXHAUSTED":
        return f"paused {what} (usage exhausted)".replace("  ", " ")
    if status == "AUTH_DEAD":
        return "credential dead — Daemon parked" + (f" · {what}" if what else "")
    if exit_code not in (0, None):
        return f"FAILED — exit {exit_code}" + (f" · {what}" if what else "")
    return what or f"exit {exit_code} (no work line)"


def fire_item(rec):
    work = rec.get("work") or {}
    outcome = rec.get("outcome") or {}
    result = rec.get("result") or {}
    return {
        "id": rec.get("fireId"),
        "kind": rec.get("kind"),
        "badge": fire_badge(rec),
        "startedAt": rec.get("startedAt"),
        "endedAt": rec.get("endedAt"),
        "inFlight": rec.get("endedAt") is None,
        "exit": rec.get("exit"),
        "phase": rec.get("phase"),
        "summary": fire_summary(rec),
        "issue": work.get("issue") if work.get("issue") is not None else rec.get("issue"),
        "pr": work.get("pr"),
        "noWork": work.get("kind") == "none",
        "outcome": outcome.get("status"),
        "resetAt": outcome.get("resetAt") or None,
        "costUsd": result.get("totalCostUsd"),
        "model": result.get("model") or rec.get("model"),
        "line": work.get("line"),
        "notes": rec.get("notes") or [],
    }


def read_records(limit=FIRES_SHOWN + 2):
    """(records newest first, unreadable file names). Fire ids start with
    their UTC timestamp, so the file names sort in start order."""
    paths = sorted(glob.glob(os.path.join(STATE_DIR, "fires", "*.json")))
    records, unreadable = [], []
    for path in reversed(paths):
        if len(records) >= limit:
            break
        try:
            with open(path) as f:
                rec = json.load(f)
            if not isinstance(rec, dict) or not rec.get("fireId"):
                raise ValueError("not a Fire record")
            records.append(rec)
        except (OSError, ValueError):
            unreadable.append(os.path.basename(path))
    records.sort(key=lambda r: (r.get("startedAt") or "", r.get("fireId") or ""), reverse=True)
    return records, sorted(unreadable)


def _started(rec):
    try:
        return datetime.fromisoformat((rec.get("startedAt") or "").replace("Z", "+00:00"))
    except ValueError:
        return None


def fetch_fires():
    records, unreadable = read_records()
    items = [fire_item(r) for r in records[:FIRES_SHOWN]]
    current = None
    if records and records[0].get("endedAt") is None:
        started = _started(records[0])
        if started and datetime.now(timezone.utc) - started < IN_FLIGHT_MAX:
            current = items[0]
    return {"items": items, "current": current, "unreadable": unreadable}


def last_finished(records):
    return next((r for r in records if r.get("endedAt") is not None), None)


def shape_bootstrap(records):
    """The warning the newest finished Fire recorded (ADR 0007: derived from
    the default-branch config each Fire). None before any Fire resolved one."""
    rec = next((r for r in records if r.get("endedAt") is not None and r.get("bootstrap") is not None), None)
    if not rec:
        return {"warning": None, "fireId": None, "notes": []}
    return {"warning": rec["bootstrap"], "fireId": rec["fireId"], "notes": rec.get("notes") or []}


# --- budget: the usage sensor's verdict, verbatim ----------------------------

_LIMIT_LABELS = {"session": "Session", "weekly": "Weekly · all models"}


def limit_tiles(verdict):
    return [
        {"key": lim.get("scope"),
         "label": _LIMIT_LABELS.get(lim.get("scope")) or f"Weekly · {lim.get('scope')}",
         "percent": lim.get("utilization"),
         "resetsAt": lim.get("resetsAt")}
        for lim in (verdict or {}).get("limits") or []
    ]


def sensor_message(verdict, rc, env_mode=None):
    """The one line the budget tile shows beside the verdict, per auth mode
    and sensor exit code (lib/usage-sensor.sh); None when the sensor spoke
    normally and there is nothing to add."""
    verdict = verdict or {}
    mode = verdict.get("authMode") or env_mode
    state = verdict.get("state")
    if rc == 4 or state == "auth-dead":
        return "credential dead — the Daemon is parked until the login is renewed"
    if rc == 5:
        return "auth mode mismatch — CLAUDE_AUTH_MODE contradicts the credential; no Fire until fixed"
    if rc == 6 or mode == "api-key":
        return "api-key auth mode has no spend pacing yet — the Daemon refuses to start"
    if mode == "setup-token":
        return "no usage sensor in this auth mode"
    if mode == "login":
        if verdict.get("sensor") not in (None, "usage-endpoint"):
            return "usage endpoint unavailable — the verdict comes from the last Fire instead"
        if state == "stale":
            return "usage endpoint unreachable — holding the last good reading"
        return None
    return "no auth mode declared in the Host env (CLAUDE_AUTH_MODE)"


def fetch_sensor():
    out = run([AUTO_AGENT_BIN, "usage-sensor"], timeout=60, check=False)
    try:
        verdict = json.loads(out.stdout)
        if not isinstance(verdict, dict):
            raise ValueError
    except ValueError:
        raise RuntimeError(f"usage-sensor exit {out.returncode} printed no verdict: {out.stderr.strip()[:200]}")
    return {"rc": out.returncode, "verdict": verdict}


def build_budget(records):
    section = dict(cached("sensor", 60, fetch_sensor))
    verdict = section.get("verdict")
    last = last_finished(records)
    section.update(
        rc=section.get("rc"),
        verdict=verdict,
        authMode=(verdict or {}).get("authMode") or AUTH_MODE,
        message=sensor_message(verdict, section.get("rc"), AUTH_MODE),
        tiles=limit_tiles(verdict),
        lastFire={"fireId": last["fireId"], "endedAt": last.get("endedAt"), "gate": last.get("gate")}
        if last else None,
    )
    return section


# --- the Daemon ---------------------------------------------------------------


def fetch_daemon():
    st = read_json(os.path.join(STATE_DIR, "daemon-state.json")) or {}
    parked = read_json(os.path.join(STATE_DIR, "parked.json"))
    unit = {"active": "unknown", "mainPid": None, "since": None}
    try:
        unit["active"] = run([SYSTEMCTL_BIN, "is-active", DAEMON_UNIT], timeout=5, check=False).stdout.strip() or "unknown"
        show = run([SYSTEMCTL_BIN, "show", DAEMON_UNIT, "-p", "MainPID", "-p", "ActiveEnterTimestamp"], timeout=5)
        for line in show.stdout.splitlines():
            k, _, v = line.partition("=")
            if k == "MainPID":
                unit["mainPid"] = int(v) if v.isdigit() and int(v) else None
            elif k == "ActiveEnterTimestamp":
                unit["since"] = v or None
    except Exception as e:
        unit["active"] = unit["active"] if unit["active"] != "unknown" else str(e)[:40]
    tail, tail_error = [], None
    try:
        # The live tail only: shown as is, never parsed.
        out = run([JOURNALCTL_BIN, f"SYSLOG_IDENTIFIER={DAEMON_UNIT}", "-n", str(TAIL_LINES),
                   "--no-pager", "-o", "cat"], timeout=10)
        tail = out.stdout.splitlines()[-TAIL_LINES:]
    except Exception as e:
        tail_error = str(e)[:200]
    state = st.get("state") or "unknown"
    return {
        "unit": unit,
        "state": state,
        "stateDetail": st.get("detail"),
        "stateAt": st.get("at"),
        "resetAt": st.get("resetAt"),
        "fail": {"count": st.get("fails"), "cap": st.get("failCap")}
        if state in ("fire_failed", "fail_cap") else None,
        "parked": parked if (parked or {}).get("parked") else None,
        "tail": tail,
        "tailError": tail_error,
    }


# --- the Target Project: config, queue, PRs ----------------------------------


def need_target():
    if not TARGET:
        raise RuntimeError("no Target Project: AUTO_AGENT_TARGET_DIR is not set in the Host env")
    return TARGET


def fetch_config():
    out = run([AUTO_AGENT_BIN, "show-config", need_target()], timeout=60)
    return {"config": json.loads(out.stdout)}


def repo_slug():
    cfg = cached("config", 300, fetch_config)
    slug = ((cfg.get("config") or {}).get("repo") or {}).get("slug")
    if not slug:
        raise RuntimeError("waiting for Harness config: " + (cfg.get("error") or "no repo slug"))
    return slug


def fetch_pipeline():
    out = run([AUTO_AGENT_BIN, "work-probe", need_target()], timeout=90)
    return {"scan": json.loads(out.stdout.strip())}


def is_docs_only(branch):
    """A research PR (the resolve lane's own, merged by the docs-only gate)."""
    return (branch or "").startswith(VOCAB["research"])


def fetch_prs():
    out = run([GH_BIN, "pr", "list", "--repo", repo_slug(), "--state", "open", "--json",
               "number,title,headRefName,labels,mergeable,isDraft,url"], timeout=30)
    return {"items": [
        {"number": p["number"], "title": p["title"], "url": p.get("url"), "branch": p["headRefName"],
         "labels": [lab["name"] for lab in p.get("labels", [])], "mergeable": p.get("mergeable"),
         "isDraft": p.get("isDraft", False), "docsOnly": is_docs_only(p["headRefName"])}
        for p in json.loads(out.stdout)
    ]}


# --- wayfinder maps -----------------------------------------------------------

# A map's Destination: a `## Destination` heading (text on the next line) or an
# inline `**Destination**:`. The word must END the label, so prose like
# "Destinations are still being scoped" is not a Destination.
_DEST_RE = re.compile(r"^(?:#{1,6}\s*)?\*{0,2}Destination\*{0,2}\s*(?::\s*(.*))?$", re.I)
# Body-text dependency, the form the slicing skills write: `Blocked by #N`.
_BODY_BLOCKER_RE = re.compile(r"Blocked by\s+#(\d+)", re.I)

# Page sizes are bounded by GitHub's static node budget (the product of every
# `first:` down each path must stay under 500,000 or the query is rejected
# with MAX_NODE_LIMIT_EXCEEDED): 20 + 20x30 + 20x30x(5+10+10) = 15,620.
# Truncation is never silent: totalCount and pageInfo flag a partial view.
MAPS_QUERY = """
query($owner: String!, $name: String!, $label: String!) {
  repository(owner: $owner, name: $name) {
    issues(first: 20, labels: [$label], states: OPEN,
           orderBy: {field: CREATED_AT, direction: DESC}) {
      totalCount
      pageInfo { hasNextPage }
      nodes {
        number title url body
        subIssues(first: 30) {
          totalCount
          pageInfo { hasNextPage }
          nodes {
            number title url state body
            assignees(first: 5) { nodes { login } }
            labels(first: 10) { nodes { name } }
            blockedBy(first: 10) { nodes { number state } }
          }
        }
      }
    }
  }
}
"""


def _destination(body):
    lines = (body or "").splitlines()
    for i, line in enumerate(lines):
        m = _DEST_RE.match(line.strip())
        if not m:
            continue
        value = (m.group(1) or "").strip()
        if not value:  # heading form: the sentence is on a following line
            for nxt in lines[i + 1:]:
                nxt = nxt.strip()
                if not nxt:
                    continue
                value = "" if nxt.startswith("#") else nxt
                break
        return value.replace("**", "").strip()
    return ""


def body_blockers(body):
    numbers = []
    for m in _BODY_BLOCKER_RE.finditer(body or ""):
        n = int(m.group(1))
        if n not in numbers:
            numbers.append(n)
    return numbers


def _issues(payload):
    """The map connection of a MAPS_QUERY payload ({} when absent)."""
    return (((payload or {}).get("data") or {}).get("repository") or {}).get("issues") or {}


def _payload_children(payload):
    for mp in _issues(payload).get("nodes") or []:
        for child in (mp.get("subIssues") or {}).get("nodes") or []:
            yield child


def _states_in_payload(payload):
    states = {}
    for child in _payload_children(payload):
        if child.get("number") is not None and child.get("state"):
            states.setdefault(child["number"], child["state"])
        for b in (child.get("blockedBy") or {}).get("nodes") or []:
            if b.get("number") is not None and b.get("state"):
                states.setdefault(b["number"], b["state"])
    return states


def unresolved_body_blockers(payload):
    known = _states_in_payload(payload)
    wanted = []
    for child in _payload_children(payload):
        for n in body_blockers(child.get("body")):
            if n not in known and n not in wanted:
                wanted.append(n)
    return sorted(wanted)


def shape_maps(payload, states=None):
    """Pure: GraphQL payload -> open maps, each with its frontier: sub-issues
    that are open, unassigned and not blocked by an open issue, natively or in
    the body. An unknown body blocker counts as open, so the card hides work
    rather than calling blocked work ready."""
    nodes = _issues(payload)
    known = dict(_states_in_payload(payload))
    known.update(states or {})
    prefix = VOCAB["wayfinder"]
    items = []
    for mp in nodes.get("nodes") or []:
        frontier = []
        subs = mp.get("subIssues") or {}
        for child in subs.get("nodes") or []:
            if child.get("state") != "OPEN":
                continue
            if (child.get("assignees") or {}).get("nodes"):
                continue
            if any(b.get("state") == "OPEN" for b in (child.get("blockedBy") or {}).get("nodes") or []):
                continue
            if any(known.get(n, "OPEN") == "OPEN" for n in body_blockers(child.get("body"))):
                continue
            labels = [lab.get("name") or "" for lab in (child.get("labels") or {}).get("nodes") or []]
            kind = next((name[len(prefix):] for name in labels if name.startswith(prefix)), None)
            frontier.append({
                "number": child.get("number"), "title": child.get("title"), "url": child.get("url"),
                "type": kind,
                "badge": "AFK" if VOCAB["afk"] in labels else "HITL" if VOCAB["hitl"] in labels else None,
            })
        sub_total = subs.get("totalCount")
        partial = bool((subs.get("pageInfo") or {}).get("hasNextPage")) or (
            isinstance(sub_total, int) and sub_total > len(subs.get("nodes") or []))
        items.append({
            "number": mp.get("number"), "title": mp.get("title"), "url": mp.get("url"),
            "destination": _destination(mp.get("body")), "frontier": frontier, "partial": partial,
        })
    total = nodes.get("totalCount")
    return {
        "items": items,
        "total": total if isinstance(total, int) else len(items),
        "truncated": bool((nodes.get("pageInfo") or {}).get("hasNextPage")) or any(m["partial"] for m in items),
    }


def _graphql(query, owner, name, **fields):
    args = [GH_BIN, "api", "graphql", "-f", f"query={query}", "-F", f"owner={owner}", "-F", f"name={name}"]
    for k, v in fields.items():
        args += ["-F", f"{k}={v}"]
    return json.loads(run(args, timeout=30).stdout)


def fetch_blocker_states(owner, name, numbers):
    """State of issues referenced only as prose `Blocked by #N`, in one call."""
    if not numbers:
        return {}
    fields = " ".join(f"i{n}: issue(number: {n}) {{ number state }}" for n in numbers[:80])
    query = ("query($owner: String!, $name: String!) { "
             f"repository(owner: $owner, name: $name) {{ {fields} }} }}")
    repo = (_graphql(query, owner, name).get("data") or {}).get("repository") or {}
    return {v["number"]: v["state"] for v in repo.values()
            if isinstance(v, dict) and v.get("number") is not None and v.get("state")}


def fetch_maps():
    owner, _, name = repo_slug().partition("/")
    payload = _graphql(MAPS_QUERY, owner, name, label=VOCAB["map"])
    try:
        states = fetch_blocker_states(owner, name, unresolved_body_blockers(payload))
    except Exception:
        states = {}  # unknown blockers read as OPEN: hide, never over-promise
    return shape_maps(payload, states)


def shape_wayfinder(scan, maps):
    """Pure: the Wayfinder tile, "N maps · M frontier · K AFK". Unknown is not
    zero: a failed maps call or a locked scan reads None, never 0."""
    maps = maps or {}
    scan = scan or {}
    unknown = bool(maps.get("error")) or maps.get("items") is None
    frontier = [t for m in maps.get("items") or [] for t in m.get("frontier") or []]
    total = maps.get("total")
    open_maps = total if isinstance(total, int) else scan.get("openMaps")
    scan_ok = not scan.get("locked") and isinstance(scan.get("slices"), int)
    return {
        "maps": open_maps if isinstance(open_maps, int) else None,
        "frontier": None if unknown else len(frontier),
        "afk": scan.get("slices") if scan_ok else None,
        "unknown": unknown,
        "truncated": bool(maps.get("truncated")),
        "queueSlices": scan.get("slices") if scan_ok else None,
        "queueWayfinder": scan.get("wayfinder") if scan_ok else None,
    }


# --- the summary of the Fire in flight (Host env toggle) ----------------------


def transcript_dir():
    """Where Claude Code keeps the Target Project's transcripts."""
    config = os.environ.get("CLAUDE_CONFIG_DIR") or os.path.expanduser("~/.claude")
    return os.path.join(config, "projects", re.sub(r"[^A-Za-z0-9]", "-", TARGET))


def _recent_transcript_events(since, max_events=40):
    """Compact activity lines from every transcript born since the Fire began
    (each subagent writes its own JSONL); only the tail of each file is read."""
    def born_after(path):
        # mtime alone also matches an interactive session in the same checkout.
        try:
            with open(path, errors="replace") as f:
                first = json.loads(f.readline())
            born = datetime.fromisoformat((first.get("timestamp") or "").replace("Z", "+00:00"))
            return born >= since - timedelta(seconds=60)
        except Exception:
            return False

    paths = [p for p in glob.glob(os.path.join(transcript_dir(), "*.jsonl"))
             if os.path.getmtime(p) >= since.timestamp() and born_after(p)]
    paths.sort(key=os.path.getmtime)
    events = []
    for p in paths[-6:]:
        size = os.path.getsize(p)
        with open(p, errors="replace") as f:
            if size > 131072:
                f.seek(size - 131072)
                f.readline()  # drop the partial line the seek landed in
            lines = f.read().splitlines()
        for raw in lines[-80:]:
            try:
                rec = json.loads(raw)
            except ValueError:
                continue
            content = (rec.get("message") or {}).get("content")
            if not isinstance(content, list):
                continue
            ts = rec.get("timestamp") or ""
            for block in content:
                if not isinstance(block, dict):
                    continue
                if block.get("type") == "tool_use":
                    inp = block.get("input") or {}
                    hint = (inp.get("description") or inp.get("command") or inp.get("prompt")
                            or inp.get("file_path") or inp.get("skill") or "")
                    events.append((ts, f"tool {block.get('name')}: {str(hint)[:110]}"))
                elif block.get("type") == "text" and rec.get("type") == "assistant":
                    text = (block.get("text") or "").strip()
                    if text:
                        events.append((ts, f"says: {text[:110]}"))
    events.sort(key=lambda e: e[0])
    return [line for _, line in events[-max_events:]]


def fetch_fire_summary():
    """A short title and description of the Fire in flight, written by a cheap
    model from the live transcript tail: the only view into a Fire while
    `claude --print` buffers. One call per cache window, only mid-Fire."""
    empty = {"enabled": SUMMARY_ENABLED, "forFire": None, "title": None, "description": None, "issue": None}
    if not SUMMARY_ENABLED:
        return empty
    cur = cached("fires", 10, fetch_fires).get("current")
    if not cur:
        return empty
    started = datetime.fromisoformat(cur["startedAt"].replace("Z", "+00:00"))
    events = _recent_transcript_events(started) if TARGET else []
    issue = None
    try:
        items = json.loads(run([GH_BIN, "issue", "list", "--repo", repo_slug(), "--label", VOCAB["inProgress"],
                                "--state", "open", "--json", "number,title"], timeout=15).stdout)
        if items:
            issue = f"issue #{items[0]['number']}: {items[0]['title']}"
    except Exception:
        pass
    if not events and not issue:
        return dict(empty, forFire=cur["id"])
    context = "\n".join(filter(None, [
        f"Fire started {cur['startedAt']} (id {cur['id']}).",
        f"Locked issue: {issue}" if issue else None,
        "Recent activity (oldest first):",
        *events,
    ]))
    prompt = (
        "You are labeling a status card for one Fire of an autonomous coding "
        "agent (an afk-pickup Fire: it picks a GitHub issue or reconciles a PR, runs "
        "an implementer with reviewer and verifier subagents, watches CI and "
        "verifies the change). Based on the activity below, reply with ONLY a "
        'JSON object {"title": "...", "description": "..."}: title under 60 '
        "chars naming the work item and phase; description 1-2 plain sentences "
        "on what is happening right now and which agent or step is active. No "
        "markdown, no code fences.\n\n" + context[:8000]
    )
    # A neutral cwd, so the Target Project's own context is not loaded.
    out = run([CLAUDE_BIN, "--model", SUMMARY_MODEL, "-p", prompt], timeout=120, cwd=tempfile.gettempdir())
    m = re.search(r"\{.*\}", out.stdout, re.S)
    if not m:
        raise RuntimeError(f"summary reply unparseable: {out.stdout.strip()[:120]}")
    parsed = json.loads(m.group(0))
    return dict(empty, forFire=cur["id"],
                title=str(parsed.get("title") or "")[:80] or None,
                description=str(parsed.get("description") or "")[:400] or None,
                issue=issue)


# --- /api/status --------------------------------------------------------------


def build_status():
    records, _ = read_records()
    maps = cached("maps", 300, fetch_maps)
    pipeline = cached("pipeline", 60, fetch_pipeline)
    try:
        repo = repo_slug()
    except Exception:
        repo = None
    return {
        "generatedAt": now_iso(),
        "host": {"bind": BIND, "port": PORT, "stateDir": STATE_DIR, "target": TARGET or None,
                 "repo": repo, "summaryEnabled": SUMMARY_ENABLED},
        "budget": build_budget(records),
        "daemon": cached("daemon", 10, fetch_daemon),
        "fires": cached("fires", 10, fetch_fires),
        "bootstrap": shape_bootstrap(records),
        "pipeline": pipeline,
        "openPrs": cached("prs", 60, fetch_prs),
        "maps": maps,
        "wayfinder": shape_wayfinder(pipeline.get("scan"), maps),
        "fireSummary": cached("fireSummary", 90, fetch_fire_summary),
    }


class Handler(BaseHTTPRequestHandler):
    server_version = "auto-agent-dashboard"

    def _send(self, code, ctype, body):
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?", 1)[0]
        if path in ("/", "/index.html"):
            try:
                with open(os.path.join(HERE, "index.html"), "rb") as f:
                    body = f.read()
            except OSError:
                self.send_error(500, "index.html missing")
                return
            self._send(200, "text/html; charset=utf-8", body)
        elif path == "/api/status":
            self._send(200, "application/json", json.dumps(build_status()).encode())
        elif path == "/favicon.ico":
            self.send_response(204)
            self.end_headers()
        else:
            self.send_error(404)

    def do_HEAD(self):
        if self.path.split("?", 1)[0] in ("/", "/index.html", "/api/status"):
            self.send_response(200)
            self.end_headers()
        else:
            self.send_error(404)

    def log_message(self, fmt, *args):  # the journal stays quiet per request
        pass


def main():
    server = ThreadingHTTPServer((BIND, PORT), Handler)
    host, port = server.server_address[:2]
    print(f"auto-agent dashboard listening on {host}:{port}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    try:
        main()
    except OSError as e:
        print(f"auto-agent dashboard: cannot bind {BIND}:{PORT}: {e}", file=sys.stderr)
        sys.exit(1)
