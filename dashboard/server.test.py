#!/usr/bin/env python3
"""Tests for dashboard/server.py.

Run: python3 dashboard/server.test.py

Strategy: the seam is `/api/status` (ADR 0006). The route tests start the
real server through `bin/auto-agent dashboard` on a State dir holding the
recorded Fire records in testdata/fires/ (one of them unreadable on purpose),
with every command it shells to stubbed: `auto-agent` (usage-sensor,
work-probe, show-config), `gh`, `systemctl`, `journalctl` and `claude`. The
response is checked against the shape documented in dashboard/README.md.
The pure shaping functions (maps, frontier, blockers, Wayfinder tile, sensor
messages) are imported and tested with no process at all; the maps tests are
carried over from Smart-Smoker-V2's dashboard-server.test.py.
"""

import importlib.util
import json
import os
import re
import shutil
import socket
import subprocess
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from datetime import datetime, timedelta, timezone

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
CLI = os.path.join(ROOT, "bin", "auto-agent")
FIRES = os.path.join(HERE, "testdata", "fires")


def load_server():
    spec = importlib.util.spec_from_file_location(
        "dashboard_server", os.path.join(HERE, "server.py")
    )
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


srv = load_server()


def documented_shape():
    with open(os.path.join(HERE, "README.md")) as f:
        text = f.read()
    m = re.search(r"## `/api/status`.*?```json\n(.*?)\n```", text, re.S)
    return json.loads(m.group(1))


def shape_errors(doc, got, path="$"):
    """Every place `got` departs from the documented example `doc`: the same
    keys in every object, list items shaped like the example's first item,
    scalars of the example's type or null. A null example accepts anything."""
    if doc is None or got is None:
        return []
    if isinstance(doc, dict):
        if not isinstance(got, dict):
            return [f"{path}: want object, got {type(got).__name__}"]
        errs = []
        if set(doc) != set(got):
            errs.append(f"{path}: keys differ: missing {sorted(set(doc) - set(got))}, "
                        f"extra {sorted(set(got) - set(doc))}")
        for k in set(doc) & set(got):
            errs += shape_errors(doc[k], got[k], f"{path}.{k}")
        return errs
    if isinstance(doc, list):
        if not isinstance(got, list):
            return [f"{path}: want list, got {type(got).__name__}"]
        errs = []
        if doc:
            for i, item in enumerate(got):
                errs += shape_errors(doc[0], item, f"{path}[{i}]")
        return errs
    number = (int, float)
    if isinstance(doc, bool):
        ok = isinstance(got, bool)
    elif isinstance(doc, number):
        ok = isinstance(got, number) and not isinstance(got, bool)
    else:
        ok = isinstance(got, type(doc))
    return [] if ok else [f"{path}: want {type(doc).__name__}, got {type(got).__name__} {got!r}"]


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


LOGIN_VERDICT = {
    "authMode": "login", "sensor": "usage-endpoint", "state": "ok",
    "remainPct": 55.5, "resetAt": "2026-09-23T15:00:00Z", "shouldFire": True,
    "observedAt": "2026-09-23T13:30:00Z",
    "limits": [
        {"scope": "session", "utilization": 44.5, "resetsAt": "2026-09-23T15:00:00Z"},
        {"scope": "weekly", "utilization": 20, "resetsAt": "2026-09-28T00:00:00Z"},
        {"scope": "fable", "utilization": 12, "resetsAt": "2026-09-28T00:00:00Z"},
    ],
    "warnings": [], "fireModel": None, "fireModelUntil": None,
}
SETUP_TOKEN_VERDICT = {
    "authMode": "setup-token", "sensor": "none", "state": "unavailable",
    "remainPct": None, "resetAt": None, "shouldFire": True,
    "observedAt": "2026-09-23T13:31:00Z", "limits": [], "warnings": [],
    "fireModel": None, "fireModelUntil": None,
}
SCAN = {"locked": True, "reconcile": None, "paused": None, "pickSig": "38",
        "prSig": "56,57", "slices": 2, "wayfinder": 1, "openMaps": 1}
PRS = [
    {"number": 56, "title": "feat(deps): the deps-land lane (#36)", "headRefName": "feat/issue-36",
     "labels": [{"name": "AFK:verify-human"}], "mergeable": "MERGEABLE", "isDraft": False,
     "url": "https://github.com/acme/widgets/pull/56"},
    {"number": 57, "title": "docs(research): gh deps (#3)", "headRefName": "research/gh-deps",
     "labels": [], "mergeable": "CONFLICTING", "isDraft": False,
     "url": "https://github.com/acme/widgets/pull/57"},
]


class Host:
    """A State dir, a Host env file and a stub for every command, then the
    real server started through `bin/auto-agent dashboard`."""

    def __init__(self, verdict=LOGIN_VERDICT, sensor_rc=0, env=None, in_flight=True):
        self.dir = tempfile.mkdtemp()
        self.state = os.path.join(self.dir, "state")
        self.bin = os.path.join(self.dir, "bin")
        os.makedirs(os.path.join(self.state, "fires"))
        os.makedirs(self.bin)
        os.makedirs(os.path.join(self.dir, "target"))
        for name in os.listdir(FIRES):
            shutil.copy(os.path.join(FIRES, name), os.path.join(self.state, "fires", name))
        if in_flight:
            # The Fire in flight: the record the wrapper writes before claude runs.
            started = datetime.now(timezone.utc) - timedelta(minutes=5)
            self.in_flight_id = started.strftime("%Y%m%dT%H%M%SZ") + "-105"
            with open(os.path.join(FIRES, "20260923T100000Z-101.json")) as f:
                rec = json.load(f)
            rec.update(fireId=self.in_flight_id, startedAt=started.strftime("%Y-%m-%dT%H:%M:%SZ"),
                       endedAt=None, exit=None, issue=None, outcome=None, result=None,
                       work={"kind": None, "issue": None, "pr": None, "slug": None,
                             "line": None, "settled": None, "pickedLine": None})
            self.write(os.path.join(self.state, "fires", self.in_flight_id + ".json"), json.dumps(rec))
        self.write(os.path.join(self.state, "daemon-state.json"), json.dumps({
            "state": "firing", "detail": "budget above min, firing", "at": "2026-09-23T13:31:00Z",
            "resetAt": None, "fails": 0, "failCap": 3, "daemonId": "host-1-1"}))
        self.write(os.path.join(self.dir, "sensor.out"), json.dumps(verdict))
        self.write(os.path.join(self.dir, "sensor.rc"), str(sensor_rc))
        self.write(os.path.join(self.dir, "prs.json"), json.dumps(PRS))
        self.write(os.path.join(self.dir, "maps.json"), json.dumps(MAPS_PAYLOAD))
        self.stub("auto-agent", f"""
echo "$*" >> {self.dir}/calls.log
case "$1" in
  usage-sensor) cat {self.dir}/sensor.out; exit $(cat {self.dir}/sensor.rc) ;;
  work-probe) echo '{json.dumps(SCAN)}' ;;
  show-config) echo '{{"repo": {{"owner": "acme", "name": "widgets", "slug": "acme/widgets"}}}}' ;;
  *) exit 2 ;;
esac""")
        self.stub("gh", f"""
echo "gh $*" >> {self.dir}/calls.log
case "$*" in
  "pr list"*) cat {self.dir}/prs.json ;;
  "api graphql"*) cat {self.dir}/maps.json ;;
  "issue list"*) echo '[{{"number": 38, "title": "Setup engine"}}]' ;;
  *) exit 1 ;;
esac""")
        self.stub("systemctl", """
case "$1" in
  is-active) echo active ;;
  show) printf 'MainPID=4242\\nActiveEnterTimestamp=Wed 2026-09-23 09:00:00 UTC\\n' ;;
esac""")
        self.stub("journalctl", """
echo "journalctl $*" >> %s/calls.log
echo '[daemon 2026-09-23T13:30:59Z] gate rc=0 sensor=usage-endpoint state=ok'
echo '[daemon 2026-09-23T13:31:00Z] budget above min, firing'""" % self.dir)
        self.stub("claude", f"""
echo "claude $*" >> {self.dir}/calls.log
echo 'Sure: {{"title": "Implementing #38", "description": "Writing the first test."}}'""")
        self.port = free_port()
        self.host_env = os.path.join(self.dir, "host.env")
        lines = [f"AUTO_AGENT_DASHBOARD_PORT={self.port}", f"AUTO_AGENT_STATE_DIR={self.state}",
                 f"AUTO_AGENT_TARGET_DIR={os.path.join(self.dir, 'target')}"]
        lines += [f"{k}={v}" for k, v in (env or {}).items()]
        self.write(self.host_env, "\n".join(lines) + "\n")
        self.proc = None

    def write(self, path, text):
        with open(path, "w") as f:
            f.write(text)

    def stub(self, name, body):
        path = os.path.join(self.bin, name)
        self.write(path, "#!/usr/bin/env bash\n" + body.lstrip("\n") + "\n")
        os.chmod(path, 0o755)

    def start(self):
        env = {k: v for k, v in os.environ.items()
               if not k.startswith(("AUTO_AGENT_", "CLAUDE_AUTH_MODE"))}
        env.update(HOME=self.dir, AUTO_AGENT_HOST_ENV=self.host_env,
                   AUTO_AGENT_BIN=os.path.join(self.bin, "auto-agent"),
                   GH_BIN=os.path.join(self.bin, "gh"), CLAUDE_BIN=os.path.join(self.bin, "claude"),
                   SYSTEMCTL_BIN=os.path.join(self.bin, "systemctl"),
                   JOURNALCTL_BIN=os.path.join(self.bin, "journalctl"))
        self.proc = subprocess.Popen(["bash", CLI, "dashboard"], env=env, text=True,
                                     stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        timer = threading.Timer(15, self.proc.kill)
        timer.start()
        self.banner = self.proc.stdout.readline().strip()
        timer.cancel()
        return self

    def get(self, path="/api/status", host="127.0.0.1"):
        with urllib.request.urlopen(f"http://{host}:{self.port}{path}", timeout=60) as resp:
            body = resp.read()
            return resp.status, (json.loads(body) if path == "/api/status" else body.decode())

    def calls(self):
        try:
            with open(os.path.join(self.dir, "calls.log")) as f:
                return f.read()
        except OSError:
            return ""

    def stop(self):
        if self.proc:
            self.proc.kill()
            self.proc.wait()
            self.proc.stdout.close()
        shutil.rmtree(self.dir, ignore_errors=True)


class StatusRouteTests(unittest.TestCase):
    """AC 1: `/api/status` served from a State dir of recorded Fire records
    matches the documented shape."""

    @classmethod
    def setUpClass(cls):
        cls.host = Host().start()
        cls.code, cls.status = cls.host.get()

    @classmethod
    def tearDownClass(cls):
        cls.host.stop()

    def test_route_answers_200_with_the_documented_shape(self):
        self.assertEqual(self.code, 200)
        self.assertEqual(shape_errors(documented_shape(), self.status), [])

    def test_history_is_the_records_newest_first_and_skips_an_unreadable_one(self):
        ids = [f["id"] for f in self.status["fires"]["items"]]
        self.assertEqual(ids, [self.host.in_flight_id, "20260923T130000Z-104", "20260923T120000Z-103",
                               "20260923T110000Z-102", "20260923T100000Z-101"])
        self.assertEqual(self.status["fires"]["unreadable"], ["20260923T090000Z-99.json"])

    def test_the_record_with_no_end_is_the_current_fire(self):
        cur = self.status["fires"]["current"]
        self.assertEqual(cur["id"], self.host.in_flight_id)
        self.assertTrue(cur["inFlight"])
        self.assertEqual(cur["summary"], "in flight")

    def test_each_record_reads_as_what_the_fire_did(self):
        by_id = {f["id"]: f for f in self.status["fires"]["items"]}
        got = {i: (by_id[i]["badge"], by_id[i]["summary"], by_id[i]["exit"], by_id[i]["noWork"])
               for i in by_id if i != self.host.in_flight_id}
        self.assertEqual(got, {
            "20260923T130000Z-104": ("research", "resolve #3 research gh-deps", 0, False),
            "20260923T120000Z-103": ("slice", "FAILED — exit 1 · #37 Dashboard per Host", 1, False),
            "20260923T110000Z-102": (None, "no work — queue empty", 0, True),
            "20260923T100000Z-101": ("slice", "#36 Deps-land lane behind its config block", 0, False),
        })

    def test_bootstrap_warning_is_the_newest_finished_record(self):
        self.assertEqual(self.status["bootstrap"], {
            "warning": True, "fireId": "20260923T130000Z-104",
            "notes": ["deployed-lane: off — verification.deployed.enabled is false"]})

    def test_budget_is_the_sensor_verdict_verbatim(self):
        b = self.status["budget"]
        self.assertEqual(b["verdict"], LOGIN_VERDICT)
        self.assertEqual((b["rc"], b["authMode"], b["message"]), (0, "login", None))
        self.assertEqual([(t["label"], t["percent"]) for t in b["tiles"]],
                         [("Session", 44.5), ("Weekly · all models", 20), ("Weekly · fable", 12)])
        self.assertIn("usage-sensor", self.host.calls())

    def test_daemon_state_is_the_state_file_and_the_journal_only_a_tail(self):
        d = self.status["daemon"]
        self.assertEqual((d["state"], d["stateDetail"]), ("firing", "budget above min, firing"))
        self.assertEqual(d["unit"], {"active": "active", "mainPid": 4242,
                                     "since": "Wed 2026-09-23 09:00:00 UTC"})
        self.assertEqual(d["tail"][-1], "[daemon 2026-09-23T13:31:00Z] budget above min, firing")
        self.assertIn("SYSLOG_IDENTIFIER=auto-agent-daemon", self.host.calls())

    def test_queue_prs_and_maps_come_from_the_config_repo(self):
        self.assertEqual(self.status["pipeline"]["scan"], SCAN)
        self.assertEqual(self.status["host"]["repo"], "acme/widgets")
        self.assertIn("gh pr list --repo acme/widgets", self.host.calls())
        self.assertEqual([(p["number"], p["docsOnly"]) for p in self.status["openPrs"]["items"]],
                         [(56, False), (57, True)])
        self.assertEqual([t["number"] for t in self.status["maps"]["items"][0]["frontier"]], [590, 591])
        self.assertEqual(self.status["wayfinder"]["frontier"], 2)

    def test_summary_of_the_fire_in_flight_is_on_by_default(self):
        s = self.status["fireSummary"]
        self.assertTrue(s["enabled"])
        self.assertEqual((s["forFire"], s["title"]), (self.host.in_flight_id, "Implementing #38"))
        self.assertIn("claude --model haiku", self.host.calls())

    def test_page_and_unknown_routes(self):
        code, html = self.host.get("/")
        self.assertEqual(code, 200)
        self.assertIn("/api/status", html)
        with self.assertRaises(urllib.error.HTTPError) as err:
            self.host.get("/nope")
        self.assertEqual(err.exception.code, 404)
        err.exception.close()
        head = urllib.request.Request(f"http://127.0.0.1:{self.host.port}/api/status", method="HEAD")
        with urllib.request.urlopen(head, timeout=10) as resp:
            self.assertEqual(resp.status, 200)


class SetupTokenHostTests(unittest.TestCase):
    """AC 3: a setup-token Host renders "no usage sensor in this auth mode"
    plus the last verdict; AC 4's default bind rides along."""

    @classmethod
    def setUpClass(cls):
        cls.host = Host(verdict=SETUP_TOKEN_VERDICT, in_flight=False,
                        env={"AUTO_AGENT_DASHBOARD_SUMMARY": "off"}).start()
        cls.code, cls.status = cls.host.get()

    @classmethod
    def tearDownClass(cls):
        cls.host.stop()

    def test_message_and_last_fire_verdict(self):
        b = self.status["budget"]
        self.assertEqual(b["message"], "no usage sensor in this auth mode")
        self.assertEqual(b["verdict"], SETUP_TOKEN_VERDICT)
        with open(os.path.join(FIRES, "20260923T130000Z-104.json")) as f:
            newest = json.load(f)
        self.assertEqual(b["lastFire"], {"fireId": newest["fireId"], "endedAt": newest["endedAt"],
                                         "gate": newest["gate"]})

    def test_page_renders_the_message_and_the_last_verdict(self):
        _, html = self.host.get("/")
        self.assertIn("b.message", html)
        self.assertIn("b.lastFire", html)

    def test_summary_toggle_off_makes_no_claude_call(self):
        s = self.status["fireSummary"]
        self.assertFalse(s["enabled"])
        self.assertIsNone(s["title"])
        self.assertNotIn("claude ", self.host.calls())

    def test_binds_loopback_by_default(self):
        self.assertEqual(self.status["host"]["bind"], "127.0.0.1")
        self.assertEqual(self.status["host"]["port"], self.host.port)
        self.assertEqual(self.host.banner, f"auto-agent dashboard listening on 127.0.0.1:{self.host.port}")


class HostEnvBindTests(unittest.TestCase):
    """AC 4: bind address and port come from the Host env."""

    def test_bind_and_port_from_the_host_env(self):
        host = Host(in_flight=False, env={"AUTO_AGENT_DASHBOARD_BIND": "127.0.0.2",
                                          "AUTO_AGENT_DASHBOARD_SUMMARY": "0"}).start()
        try:
            self.assertEqual(host.banner, f"auto-agent dashboard listening on 127.0.0.2:{host.port}")
            code, status = host.get(host="127.0.0.2")
            self.assertEqual((code, status["host"]["bind"]), (200, "127.0.0.2"))
            with self.assertRaises(OSError):
                host.get(host="127.0.0.1")
        finally:
            host.stop()


class DegradeTests(unittest.TestCase):
    """Every section degrades on its own; the route never 500s."""

    def test_sensor_without_a_verdict_and_gh_down(self):
        host = Host(in_flight=False, env={"AUTO_AGENT_DASHBOARD_SUMMARY": "0",
                                          "CLAUDE_AUTH_MODE": "setup-token"})
        host.write(os.path.join(host.dir, "sensor.out"), "not json")
        host.write(os.path.join(host.dir, "sensor.rc"), "1")
        host.stub("gh", "exit 1")
        host.start()
        try:
            code, status = host.get()
            self.assertEqual(code, 200)
            b = status["budget"]
            self.assertTrue(b["stale"])
            self.assertIn("usage-sensor exit 1", b["error"])
            # No verdict: the Host env's auth mode still picks the message, and
            # the last Fire's verdict is still shown.
            self.assertEqual(b["message"], "no usage sensor in this auth mode")
            self.assertEqual(b["lastFire"]["fireId"], "20260923T130000Z-104")
            self.assertTrue(status["openPrs"]["stale"])
            self.assertEqual(len(status["fires"]["items"]), 4)
        finally:
            host.stop()


class SensorMessageTests(unittest.TestCase):
    """Behaviour 2: one line per auth mode, never a blank tile."""

    def msg(self, verdict, rc=0, env_mode=None):
        return srv.sensor_message(verdict, rc, env_mode)

    def test_per_auth_mode(self):
        self.assertIsNone(self.msg(LOGIN_VERDICT))
        self.assertEqual(self.msg(SETUP_TOKEN_VERDICT), "no usage sensor in this auth mode")
        self.assertEqual(self.msg(dict(SETUP_TOKEN_VERDICT, sensor="stream-events", state="stale")),
                         "no usage sensor in this auth mode")
        self.assertRegex(self.msg(dict(LOGIN_VERDICT, authMode="api-key", sensor="spend",
                                       state="unavailable", shouldFire=False), 6), "^api-key auth mode")
        self.assertRegex(self.msg(dict(LOGIN_VERDICT, state="auth-dead"), 4), "^credential dead")
        self.assertRegex(self.msg(LOGIN_VERDICT, 5), "^auth mode mismatch")
        self.assertRegex(self.msg(dict(LOGIN_VERDICT, state="stale")), "^usage endpoint unreachable")
        self.assertRegex(self.msg(dict(LOGIN_VERDICT, sensor="stream-events", state="stale")),
                         "^usage endpoint unavailable")
        self.assertRegex(self.msg(dict(LOGIN_VERDICT, authMode=None)), "^no auth mode")

    def test_host_env_mode_when_there_is_no_verdict(self):
        self.assertEqual(self.msg(None, 1, "setup-token"), "no usage sensor in this auth mode")

    def test_no_python_gate_remains(self):
        # AC 2: the budget is the sensor's; nothing here reads the usage
        # endpoint, the credential or decides shouldFire.
        with open(os.path.join(HERE, "server.py")) as f:
            src = f.read()
        for needle in ("oauth/usage", "credentials.json", "accessToken", "GATE_MIN_PCT"):
            self.assertNotIn(needle, src)
        self.assertNotRegex(src, r"[\"']shouldFire[\"']\s*:")


MAPS_PAYLOAD = {
    "data": {"repository": {"issues": {"nodes": [{
        "number": 575,
        "title": "Wayfinder: adopt AFK planning front-end",
        "url": "https://github.com/o/r/issues/575",
        "body": "## Destination\n\nReached: a spec ready for /to-tickets.\n\n## Notes\n",
        "subIssues": {"nodes": [
            {"number": 590, "title": "Research: rotate VAPID keys", "url": "https://github.com/o/r/issues/590",
             "state": "OPEN", "assignees": {"nodes": []},
             "labels": {"nodes": [{"name": "AFK"}, {"name": "wayfinder:research"}]},
             "blockedBy": {"nodes": []}},
            {"number": 591, "title": "Decide: theming tokens", "url": "https://github.com/o/r/issues/591",
             "state": "OPEN", "assignees": {"nodes": []},
             "labels": {"nodes": [{"name": "HITL"}, {"name": "wayfinder:grilling"}]},
             "blockedBy": {"nodes": [{"number": 576, "state": "CLOSED"}]}},
            {"number": 592, "title": "Blocked ticket", "url": "https://github.com/o/r/issues/592",
             "state": "OPEN", "assignees": {"nodes": []}, "labels": {"nodes": [{"name": "AFK"}]},
             "blockedBy": {"nodes": [{"number": 590, "state": "OPEN"}]}},
            {"number": 593, "title": "Claimed ticket", "url": "https://github.com/o/r/issues/593",
             "state": "OPEN", "assignees": {"nodes": [{"login": "someone"}]},
             "labels": {"nodes": [{"name": "AFK"}]}, "blockedBy": {"nodes": []}},
            {"number": 576, "title": "Done ticket", "url": "https://github.com/o/r/issues/576",
             "state": "CLOSED", "assignees": {"nodes": []}, "labels": {"nodes": [{"name": "AFK"}]},
             "blockedBy": {"nodes": []}},
        ]},
    }]}}}
}


def child(number, **kw):
    node = {"number": number, "title": f"Ticket {number}", "url": f"https://github.com/o/r/issues/{number}",
            "state": "OPEN", "body": "", "assignees": {"nodes": []},
            "labels": {"nodes": [{"name": "AFK"}]}, "blockedBy": {"nodes": []}}
    node.update(kw)
    return node


def payload(children, **subs):
    sub = {"nodes": children}
    sub.update(subs)
    return {"data": {"repository": {"issues": {"nodes": [
        {"number": 1, "title": "Map", "url": "u", "body": "", "subIssues": sub}]}}}}


class MapsShapingTests(unittest.TestCase):
    def setUp(self):
        self.maps = srv.shape_maps(MAPS_PAYLOAD)["items"]

    def test_title_destination_and_frontier(self):
        m = self.maps[0]
        self.assertEqual((m["number"], m["destination"]), (575, "Reached: a spec ready for /to-tickets."))
        self.assertEqual([t["number"] for t in m["frontier"]], [590, 591])
        research, grilling = m["frontier"]
        self.assertEqual((research["type"], research["badge"]), ("research", "AFK"))
        self.assertEqual((grilling["type"], grilling["badge"]), ("grilling", "HITL"))

    def test_destination_forms(self):
        def dest(body):
            return srv.shape_maps({"data": {"repository": {"issues": {"nodes": [
                {"number": 1, "title": "m", "url": "u", "body": body, "subIssues": {"nodes": []}}]}}}}
            )["items"][0]["destination"]
        self.assertEqual(dest("**Destination**: ship the thing\n"), "ship the thing")
        self.assertEqual(dest("Destinations are still being scoped\n"), "")
        self.assertEqual(dest("## Destination\n\n## Frontier\n- #2\n"), "")
        self.assertEqual(dest("## Notes\n- nothing\n"), "")

    def test_body_blockers(self):
        def frontier(children, states=None):
            return [t["number"] for t in srv.shape_maps(payload(children), states)["items"][0]["frontier"]]
        blocked = child(561, body="Blocked by #559\n")
        self.assertEqual(frontier([child(559), blocked]), [559])
        self.assertEqual(frontier([child(559, state="CLOSED"), blocked]), [561])
        self.assertEqual(frontier([child(561, body="Blocked by #999\n")], {999: "CLOSED"}), [561])
        self.assertEqual(frontier([child(561, body="Blocked by #999\n")]), [])
        self.assertEqual(srv.unresolved_body_blockers(payload([
            child(559, state="CLOSED"), child(561, body="Blocked by #559 and Blocked by #999\n")])), [999])
        self.assertEqual(srv.body_blockers("Blocked by #12\nBlocked by  #7\nBlocked by #12"), [12, 7])

    def test_truncation_is_never_silent(self):
        pl = payload([child(2)], totalCount=99, pageInfo={"hasNextPage": True})
        pl["data"]["repository"]["issues"]["totalCount"] = 42
        shaped = srv.shape_maps(pl)
        self.assertEqual(shaped["total"], 42)
        self.assertTrue(shaped["items"][0]["partial"])
        self.assertTrue(shaped["truncated"])

    def test_wayfinder_tile_unknown_is_not_zero(self):
        tile = srv.shape_wayfinder({"openMaps": 3, "slices": 2, "wayfinder": 1},
                                   {"stale": True, "error": "gh exit 1: boom"})
        self.assertTrue(tile["unknown"])
        self.assertEqual((tile["frontier"], tile["afk"], tile["maps"]), (None, 2, 3))
        locked = srv.shape_wayfinder({"locked": True, "slices": 0}, dict(srv.shape_maps(MAPS_PAYLOAD), error=None))
        self.assertIsNone(locked["afk"])

    def test_maps_query_is_under_githubs_node_limit(self):
        stack, pending, total = [1], None, 0
        for token in re.finditer(r"first:\s*(\d+)|[{}]", srv.MAPS_QUERY):
            if token.group(1):
                pending = int(token.group(1))
            elif token.group(0) == "{":
                mult = stack[-1] * pending if pending else stack[-1]
                if pending:
                    total += mult
                    pending = None
                stack.append(mult)
            else:
                stack.pop()
        self.assertGreater(total, 0)
        self.assertLess(total, 500_000)


class PageTests(unittest.TestCase):
    def test_every_badge_colour_has_a_rule_and_no_project_literal(self):
        with open(os.path.join(HERE, "index.html")) as f:
            html = f.read()
        colours = set(re.findall(r'\w+:\s*\["[^"]*",\s*"(\w+)"', html))
        self.assertIn("purple", colours)
        for colour in colours:
            self.assertRegex(html, r"\.badge\.%s\s*\{" % colour)
        self.assertNotRegex(html, r"(?i)smoker")


if __name__ == "__main__":
    unittest.main(verbosity=2)
