"""Isolated native lab for terminal-session conversation continuity.

Creates a private HIVERYN_HOME, one project per provider (architect + fixture
repo; "lab" for the first provider, "lab-<provider>" for the rest),
per-provider config roots with read-only copies of existing credentials
(the Claude login of an explicitly named config dir, ~/.codex-personal/auth.json,
the DeepSeek entry of the OpenCode auth file) and a loopback notification sink, then runs a hiverynd
built from this checkout. Nothing touches the live daemon, global provider
settings or existing conversations; OpenCode auto-update is disabled and
secret-like environment variables are scrubbed. Credentials are copied, never
printed, and removed by `cleanup`.

RUN_DIR must be outside $HOME: Claude Code walks parent directories for
CLAUDE.md files (including ~/.claude/CLAUDE.md) and would load private
instructions. It also carries the ownership marker the desktop's isolated
verification entry requires, so that build can be pointed at it
(HIVERYN_ISOLATED_RUN=RUN_DIR, HIVERYN_HOME=RUN_DIR/home).

Usage (from the daemon checkout):
  python3 scripts/continuity_lab.py setup RUN_DIR [claude codex opencode] [--claude-config DIR]
  python3 scripts/continuity_lab.py start RUN_DIR          # builds and starts hiverynd
  python3 scripts/continuity_lab.py stop RUN_DIR
  python3 scripts/continuity_lab.py cleanup RUN_DIR        # stop + remove copied credentials

As a module: lab = Lab(RUN_DIR); lab.call(...); lab.type(sid, text); ...
"""
import hashlib, json, os, pathlib, shutil, signal, socket, subprocess, sys, time, urllib.request, urllib.error

DAEMON = pathlib.Path(__file__).resolve().parent.parent
HOME = pathlib.Path.home()


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Lab:
    def __init__(self, root):
        self.root = pathlib.Path(root).resolve()
        self.state_path = self.root / "lab.json"
        self.state = json.loads(self.state_path.read_text()) if self.state_path.exists() else {}

    @property
    def base(self):
        return "http://127.0.0.1:%d" % self.state["port"]

    def save(self):
        self.state_path.write_text(json.dumps(self.state, indent=2))

    # ---------------------------------------------------------------- setup
    def setup(self, providers, claude_config=None):
        self.root.mkdir(parents=True, exist_ok=True, mode=0o700)
        home = self.root / "home"
        home.mkdir(parents=True, exist_ok=True)
        # The desktop's isolated verification entrypoint refuses any run
        # directory without this ownership marker.
        (self.root / "owned-isolation").write_text("workspace-migration-ticket\n")
        port, notify = free_port(), free_port()
        self.state.update(port=port, notify_port=notify, providers=providers, credentials=[])
        # One project per provider ("lab" for the first, "lab-<provider>" for
        # the rest), each with its own workspace and fixture repo, so every
        # provider can run an architect and a worker.
        registry = {}
        for i, kind in enumerate(providers):
            key = "lab" if i == 0 else "lab-" + kind
            ws, repo = self.root / ("workspace" if i == 0 else "workspace-" + kind), self.root / ("repo" if i == 0 else "repo-" + kind)
            for d in (ws / "workflows", repo):
                d.mkdir(parents=True, exist_ok=True)
            if not (repo / ".git").exists():
                subprocess.run(["git", "init", "-q", str(repo)], check=True)
                (repo / "README.md").write_text("Synthetic continuity fixture. Nothing to implement.\n")
                subprocess.run(["git", "-C", str(repo), "add", "."], check=True)
                subprocess.run(["git", "-C", str(repo), "-c", "user.name=lab", "-c", "user.email=lab@example.invalid", "commit", "-qm", "fixture"], check=True)
            (ws / "hiveryn.yaml").write_text("name: Continuity lab (%s)\nrepos:\n  fixture: %s\n" % (kind, repo))
            (ws / "OVERVIEW.md").write_text("---\nupdated_at: 2026-10-27T02:00:00Z\n---\n"
                                           "Synthetic continuity lab. Never inspect files outside this workspace and the fixture repo. "
                                           "Answer conversational messages briefly; do not start work unless asked.\n")
            registry[key] = ws
        self.state["projects"] = {("lab" if i == 0 else "lab-" + kind): kind for i, kind in enumerate(providers)}
        (home / "config.yaml").write_text("port: %d\nbind_address: 127.0.0.1\nlog_level: info\nintent_wait_timeout: 20\n"
                                         "notifications:\n  ntfy:\n    server: http://127.0.0.1:%d\n    topic: lab\n" % (port, notify))
        # The desktop requires a right-pane layout per session type.
        (home / "tabs.yaml").write_text("architect:\n  - type: kanban\n  - type: event-log\n  - type: files\n"
                                       "ticket:\n  - type: ticket\n  - type: git-diff\n  - type: event-log\n")
        (home / "architects.yaml").write_text("".join("%s: %s\n" % (k, v) for k, v in registry.items()))
        repo, ws = self.root / "repo", self.root / "workspace"
        variants = {}
        for kind in providers:
            config = self.root / kind
            config.mkdir(exist_ok=True)
            env, model = {}, ""
            if kind == "codex":
                cred = config / "auth.json"
                shutil.copyfile(HOME / ".codex-personal/auth.json", cred)
                trusted = [self.root] + [p for p in self.root.iterdir() if p.name.startswith(("repo", "workspace"))]
                (config / "config.toml").write_text(
                    "".join('[projects."%s"]\ntrust_level = "trusted"\n\n' % p for p in trusted)
                    + "[tui]\nscreen_reader_detection_done = true\n")
                env, model = {"CODEX_HOME": str(config)}, "gpt-5.6-sol"
            elif kind == "claude":
                # An explicit, authenticated login (e.g. ~/.claude): its token is
                # copied read-only and must outlive the lab, so the lab never
                # refreshes it (a refresh could rotate the original login away).
                if not claude_config:
                    raise SystemExit("claude needs --claude-config DIR (an authenticated CLAUDE_CONFIG_DIR)")
                source = pathlib.Path(claude_config).expanduser().resolve()
                state = source / ".claude.json" if source != HOME / ".claude" else HOME / ".claude.json"
                original = json.loads(state.read_text())
                keep = {k: original[k] for k in ["oauthAccount", "hasCompletedOnboarding", "lastOnboardingVersion", "userID"] if k in original}
                keep["bypassPermissionsModeAccepted"] = True
                keep["projects"] = {str(p): {"hasTrustDialogAccepted": True, "hasCompletedProjectOnboarding": True}
                                    for p in self.root.iterdir() if p.name.startswith(("repo", "workspace"))}
                (config / ".claude.json").write_text(json.dumps(keep))
                cred = config / ".credentials.json"
                service = "Claude Code-credentials"
                if source != HOME / ".claude":
                    service += "-" + hashlib.sha256(str(source).encode()).hexdigest()[:8]
                token = subprocess.run(["security", "find-generic-password", "-s", service, "-w"], capture_output=True, check=True).stdout.strip()
                expires = json.loads(token).get("claudeAiOauth", {}).get("expiresAt", 0) / 1000
                if expires - time.time() < 2 * 3600:
                    raise SystemExit("the %s login expires in under 2 hours; refresh it with claude first" % source)
                cred.write_bytes(token)
                env, model = {"CLAUDE_CONFIG_DIR": str(config), "DISABLE_AUTOUPDATER": "1"}, "sonnet"
            else:
                for key in ["DATA", "CONFIG", "CACHE", "STATE"]:
                    (config / key.lower()).mkdir(exist_ok=True)
                    env["XDG_" + key + "_HOME"] = str(config / key.lower())
                (config / "data/opencode").mkdir(exist_ok=True)
                cred = config / "data/opencode/auth.json"
                auth = json.loads((HOME / ".local/share/opencode/auth.json").read_text())
                cred.write_text(json.dumps({"deepseek": auth["deepseek"]}))
                model = "deepseek/deepseek-v4-flash"
                env["OPENCODE_DISABLE_MODELS_FETCH"] = "true"
                env["OPENCODE_DISABLE_AUTOUPDATE"] = "true"
                # The daemon installs its hook plugin in the default config root
                # only; the isolated XDG config needs the same managed file.
                catalog = HOME / ".cache/opencode/models.json"
                if catalog.exists():  # public model catalog; fetching stays disabled
                    (config / "cache/opencode").mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(catalog, config / "cache/opencode/models.json")
                plugin = HOME / ".config/opencode/plugins/agentruntime-hiveryn-daemon.ts"
                if plugin.exists():
                    (config / "config/opencode/plugins").mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(plugin, config / "config/opencode/plugins" / plugin.name)
            cred.chmod(0o600)
            self.state["credentials"].append(str(cred))
            variants[kind] = {"agent": kind, "model": model, "yolo": True, "env": env}
        (home / "variants.yaml").write_text(json.dumps(variants))
        self.save()

    # -------------------------------------------------------------- daemon
    def build(self):
        out = self.root / "hiverynd"
        subprocess.run(["go", "build", "-o", str(out), "./cmd/hiverynd"], cwd=DAEMON, check=True)
        return out

    def start(self, build=True):
        binary = self.build() if build else self.root / "hiverynd"
        secretish = ("TOKEN", "KEY", "SECRET", "PASSWORD", "CREDENTIAL")
        env = {k: v for k, v in os.environ.items()
               if not k.startswith(("HIVERYN", "AGENTRUNTIME", "CODEX", "CLAUDE", "ANTHROPIC", "OPENAI", "OPENCODE", "HF_"))
               and not any(word in k.upper() for word in secretish)}
        env["HIVERYN_HOME"] = str(self.root / "home")
        self.start_sink()
        log = (self.root / "daemon.stdout").open("a")
        p = subprocess.Popen([str(binary), "serve"], env=env, stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        self.state["pid"] = p.pid
        self.save()
        self.wait(lambda: self.call("/api/health"), 20)
        return p.pid

    SINK = (
        "import http.server,json,sys,time\n"
        "out=open(sys.argv[2],'a')\n"
        "class H(http.server.BaseHTTPRequestHandler):\n"
        " def do_POST(self):\n"
        "  body=self.rfile.read(int(self.headers.get('Content-Length') or 0)).decode('utf8','replace')\n"
        "  out.write(json.dumps({'at':time.time(),'path':self.path,'body':body})+'\\n');out.flush()\n"
        "  self.send_response(200);self.end_headers();self.wfile.write(b'{}')\n"
        " def log_message(self,*a):pass\n"
        "http.server.ThreadingHTTPServer(('127.0.0.1',int(sys.argv[1])),H).serve_forever()\n")

    def start_sink(self):
        """Loopback ntfy stand-in: records phone notifications to notifications.jsonl."""
        pid = self.state.get("sink_pid")
        if pid:
            try:
                os.kill(pid, 0)
                return
            except ProcessLookupError:
                pass
        p = subprocess.Popen([sys.executable, "-c", self.SINK, str(self.state["notify_port"]), str(self.root / "notifications.jsonl")], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        self.state["sink_pid"] = p.pid
        self.save()

    def stop(self):
        pid = self.state.get("pid")
        if not pid:
            return
        try:
            os.kill(pid, signal.SIGTERM)
            for _ in range(150):
                os.kill(pid, 0)
                time.sleep(0.1)
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        self.state["pid"] = None
        self.save()

    def cleanup(self):
        self.stop()
        if self.state.get("sink_pid"):
            try:
                os.kill(self.state["sink_pid"], signal.SIGTERM)
            except ProcessLookupError:
                pass
            self.state["sink_pid"] = None
            self.save()
        for c in self.state.get("credentials", []):
            pathlib.Path(c).unlink(missing_ok=True)

    # ----------------------------------------------------------------- api
    def call(self, path, body=None, method=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(self.base + path, data=data, method=method, headers={"Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=130) as r:
                result = json.load(r)
        except urllib.error.HTTPError as e:
            raise RuntimeError("%s %s: HTTP %d %s" % (method or ("POST" if body is not None else "GET"), path, e.code, e.read().decode()))
        if result.get("error"):
            raise RuntimeError(result["error"])
        return result["data"]

    @staticmethod
    def wait(fn, seconds=90, every=0.5):
        end, last = time.monotonic() + seconds, None
        while time.monotonic() < end:
            try:
                value = fn()
                if value:
                    return value
            except Exception as e:  # noqa: BLE001 - surfaced on timeout
                last = e
            time.sleep(every)
        raise RuntimeError("timeout: %s" % last)

    def session(self, sid):
        return self.call("/api/sessions/" + sid)

    # ------------------------------------------------------------ terminal
    def ws_url(self, sid):
        s = self.session(sid)
        tid = s["current_run"]["main_terminal_id"]
        return "ws://127.0.0.1:%d/ws/session/%s/terminal/%s?cols=120&rows=40" % (self.state["port"], sid, tid)

    def type(self, sid, text, enter=True, settle=1.0):
        """Writes into the session's main terminal through the same WebSocket as
        the desktop, as an xterm bracketed paste followed by Enter."""
        import websocket
        ws = websocket.create_connection(self.ws_url(sid), timeout=10)
        try:
            ws.send("\x1b[200~" + text + "\x1b[201~")
            if enter:
                time.sleep(settle)
                ws.send("\r")
            time.sleep(0.3)
        finally:
            ws.close()

    def screen(self, sid, seconds=1.0):
        """Returns raw replay + output for diagnostics (not history)."""
        import websocket
        ws = websocket.create_connection(self.ws_url(sid), timeout=seconds)
        chunks, end = [], time.monotonic() + seconds
        try:
            while time.monotonic() < end:
                try:
                    chunks.append(ws.recv())
                except Exception:  # noqa: BLE001 - timeout ends the capture
                    break
        finally:
            ws.close()
        return b"".join(c if isinstance(c, bytes) else c.encode() for c in chunks)


if __name__ == "__main__":
    cmd, root = sys.argv[1], sys.argv[2]
    lab = Lab(root)
    if cmd == "setup":
        args, claude_config = sys.argv[3:], None
        if "--claude-config" in args:
            i = args.index("--claude-config")
            claude_config, args = args[i + 1], args[:i] + args[i + 2:]
        lab.setup(args or ["claude", "codex", "opencode"], claude_config)
    elif cmd == "start":
        print(lab.start())
    elif cmd == "stop":
        lab.stop()
    elif cmd == "cleanup":
        lab.cleanup()
    print(json.dumps({k: v for k, v in lab.state.items() if k != "credentials"}))
