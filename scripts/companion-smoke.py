#!/usr/bin/env python3
"""Disposable, isolated companion smoke: a real daemon, a Docker sshd machine, SSH/tmux attachment and
reverse tunnel, the per-session authenticated gateway, local PTYs and stdio MCP.

FIXTURE PROVIDERS, NOT REAL CLAUDE/CODEX: `claude`/`codex` are node scripts that use the hooks, waiter,
MCP servers and (Codex) app-server queue the real adapters configure. Real-provider behaviour is not
established by this script.

Isolation: HIVERYN_HOME, HOME, CLAUDE_CONFIG_DIR, CODEX_HOME, SSH config/keys, ports and the container
are owned by the run and removed with it. Never reads live Hiveryn, provider or SSH configuration.
Requires Docker, Go, node, Python 3, OpenSSH.
"""
import http.server, json, os, pathlib, shlex, socket, subprocess, sys, tempfile, threading, time, urllib.request, uuid

ROOT = pathlib.Path(__file__).resolve().parents[1]

def run(*args, **kw):
    return subprocess.run(args, check=True, text=True, capture_output=True, **kw).stdout.strip()
def free_port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0)); return s.getsockname()[1]
def wait(fn, timeout=60, what=''):
    end = time.monotonic() + timeout; last = None
    while time.monotonic() < end:
        try:
            value = fn()
            if value: return value
        except Exception as e: last = e
        time.sleep(.25)
    raise AssertionError(f'timed out waiting for {what}: {last}')
def step(text): print('PASS', text, flush=True)

FIXTURE = r'''#!/usr/bin/env node
// Fixture provider (not Claude or Codex). See companion-smoke.py.
const fs=require('fs'),path=require('path'),cp=require('child_process'),os=require('os'),crypto=require('crypto');
const argv=process.argv.slice(2);
const agent=process.env.FIXTURE_AGENT;
const codexHome=process.env.CODEX_HOME||path.join(os.homedir(),'.codex');
const claudeHome=process.env.CLAUDE_CONFIG_DIR||path.join(os.homedir(),'.claude');
const STATE=process.env.FIXTURE_STATE||os.homedir();
const queueDir=path.join(codexHome,'fixture-queue');
if(argv[0]==='app-server'){
 fs.mkdirSync(queueDir,{recursive:true});
 const rl=require('readline').createInterface({input:process.stdin});
 rl.on('line',l=>{let r;try{r=JSON.parse(l)}catch{return}if(r.id===undefined)return;let result={};
  if(r.method==='thread/queue/list'){result={data:fs.readdirSync(queueDir).filter(f=>f.endsWith('.json')).map(f=>JSON.parse(fs.readFileSync(path.join(queueDir,f)))).filter(i=>i.threadId===r.params.threadId).map(i=>({id:i.id,clientUserMessageId:i.clientUserMessageId})),nextCursor:null}}
  else if(r.method==='thread/queue/add'){const item={id:'qs-'+crypto.randomUUID(),threadId:r.params.threadId,clientUserMessageId:r.params.clientUserMessageId,text:(r.params.input||[]).map(x=>x.text).join('\n')};
   fs.writeFileSync(path.join(queueDir,item.id+'.tmp'),JSON.stringify(item));fs.renameSync(path.join(queueDir,item.id+'.tmp'),path.join(queueDir,item.id+'.json'));result={queuedSubmission:{id:item.id}}}
  process.stdout.write(JSON.stringify({jsonrpc:'2.0',id:r.id,result})+'\n')});
 rl.on('close',()=>process.exit(0));
 return;
}
const id=process.env.AGENTRUNTIME_SESSION_ID, endpoint=process.env.AGENTRUNTIME_HOOK_ENDPOINT||'', native='native-'+id;
const role=process.env.FIXTURE_ROLE||'companion';
const log=(n,v)=>fs.appendFileSync(path.join(STATE,n+'-'+id),(typeof v==='string'?v:JSON.stringify(v))+'\n');
fs.appendFileSync(path.join(STATE,'launches'),id+'\n');
const settingsArg=argv.indexOf('--settings')>=0?JSON.parse(fs.readFileSync(argv[argv.indexOf('--settings')+1])):null;
function hook(event,extra){
 const file=agent==='claude'?path.join(claudeHome,'settings.json'):path.join(codexHome,'hooks.json');
 const cmd=JSON.parse(fs.readFileSync(file)).hooks[event][0].hooks[0].command;
 cp.execSync(cmd,{input:JSON.stringify(Object.assign({hook_event_name:event,session_id:native,cwd:process.cwd()},extra||{})),env:process.env,stdio:['pipe','ignore','ignore']});
}
function record(text,ref){
 const ts=new Date().toISOString(),u=crypto.randomUUID();
 if(agent==='claude'){const f=path.join(claudeHome,'projects','fixture',native+'.jsonl');fs.mkdirSync(path.dirname(f),{recursive:true});
  fs.appendFileSync(f,[{type:'user',uuid:u,promptId:'p-'+u,turnOrigin:'task_notification',timestamp:ts,message:{role:'user',content:text}},
   {type:'assistant',uuid:'a-'+u,promptId:'p-'+u,timestamp:ts,message:{role:'assistant',content:[{type:'text',text:'ok'}]}},
   {type:'system',subtype:'turn_duration',uuid:'s-'+u,timestamp:ts}].map(x=>JSON.stringify(x)).join('\n')+'\n');return}
 const f=path.join(codexHome,'sessions','2026','10','30','rollout-2026-10-30T10-00-00-'+native+'.jsonl');fs.mkdirSync(path.dirname(f),{recursive:true});
 if(!fs.existsSync(f))fs.writeFileSync(f,JSON.stringify({timestamp:ts,type:'session_meta',payload:{id:native,timestamp:ts}})+'\n');
 fs.appendFileSync(f,[{timestamp:ts,type:'event_msg',payload:{type:'task_started',turn_id:'t-'+u,turn_attribution:{turn_trigger:'queue'}}},
  {timestamp:ts,type:'event_msg',payload:{type:'item_completed',turn_id:'t-'+u,item:{type:'UserMessage',id:'um-'+u,client_id:ref,content:[{type:'text',text}]}}},
  {timestamp:ts,type:'event_msg',payload:{type:'item_completed',turn_id:'t-'+u,item:{type:'AgentMessage',id:'am-'+u,content:[{type:'text',text:'ok'}]}}},
  {timestamp:ts,type:'event_msg',payload:{type:'task_complete',turn_id:'t-'+u}}].map(x=>JSON.stringify(x)).join('\n')+'\n');
}
// MCP: the remote gateway over the reverse tunnel, or the local stdio server from --mcp-config.
let rpcId=0,stdio=null;const pending={};
function startStdio(){
 const cfg=JSON.parse(fs.readFileSync(argv[argv.indexOf('--mcp-config')+1])).mcpServers['hiveryn-daemon'];
 stdio=cp.spawn(cfg.command,cfg.args||[],{env:Object.assign({},process.env,cfg.env||{}),stdio:['pipe','pipe','ignore']});
 require('readline').createInterface({input:stdio.stdout}).on('line',l=>{try{const m=JSON.parse(l);if(pending[m.id]){pending[m.id](m);delete pending[m.id]}}catch{}});
}
function stdioRPC(method,params){return new Promise(res=>{const i=++rpcId;pending[i]=res;stdio.stdin.write(JSON.stringify({jsonrpc:'2.0',id:i,method,params})+'\n')})}
async function rpc(method,params){
 if(endpoint.includes('/hooks/')){
  const r=await fetch(endpoint.split('/hooks/')[0]+'/mcp',{method:'POST',headers:{'Authorization':'Bearer '+process.env.HIVERYN_WORKER_TOKEN,'Content-Type':'application/json','Accept':'application/json, text/event-stream'},body:JSON.stringify({jsonrpc:'2.0',id:++rpcId,method,params})});
  const body=await r.text();const data=body.startsWith('event:')||body.startsWith('data:')?JSON.parse(body.split('\n').find(s=>s.startsWith('data:')).slice(5)):JSON.parse(body);
  if(data.error)throw Error(JSON.stringify(data.error));return data.result}
 if(!stdio){startStdio();await stdioRPC('initialize',{protocolVersion:'2025-06-18',capabilities:{},clientInfo:{name:'fixture',version:'0'}});stdio.stdin.write(JSON.stringify({jsonrpc:'2.0',method:'notifications/initialized'})+'\n')}
 const m=await stdioRPC(method,params);if(m.error)throw Error(JSON.stringify(m.error));return m.result}
async function call(name,args){const r=await rpc('tools/call',{name,arguments:args});return r.isError?{error:r.content[0].text}:r.structuredContent}
async function onMessage(text){
 log('inbox',text);
 if(role!=='companion')return;
 const shutdown=/request_id: ([0-9a-f-]{36})/.exec(text);
 if(shutdown){while(fs.existsSync(path.join(STATE,'hold-'+id)))await new Promise(r=>setTimeout(r,200));
  log('completed',await call('completeCompanionShutdown',{request_id:shutdown[1],handoff:'Fixture handoff for '+id}));return}
 const m=/From the architect \(message_id ([0-9a-f-]{36})/.exec(text);
 if(m)log('replied',await call('replyToArchitectMessage',{message_id:m[1],message:'ack '+m[1]}));
}
function waitOnce(){return new Promise(res=>{const p=cp.spawn('sh',['-c',settingsArg.hooks.Stop[0].hooks[0].command],{env:process.env});let err='';p.stderr.on('data',d=>err+=d);
 p.on('exit',code=>res(code===2?err.trim():null));p.stdin.end(JSON.stringify({hook_event_name:'Stop',session_id:native}))})}
async function claudeLoop(){for(;;){try{hook('Stop',{stop_hook_active:false})}catch(e){log('error',String(e))}
 const msg=await waitOnce();if(msg){record(msg);await onMessage(msg)}else await new Promise(r=>setTimeout(r,500))}}
async function codexLoop(){for(;;){fs.mkdirSync(queueDir,{recursive:true});
 for(const f of fs.readdirSync(queueDir).filter(f=>f.endsWith('.json')).sort()){const item=JSON.parse(fs.readFileSync(path.join(queueDir,f)));if(item.threadId!==native)continue;
  fs.renameSync(path.join(queueDir,f),path.join(queueDir,f+'.done'));record(item.text,item.clientUserMessageId);
  try{hook('Stop',{})}catch(e){log('error',String(e))}await onMessage(item.text)}
 await new Promise(r=>setTimeout(r,300))}}
async function actions(){for(;;){const f=path.join(STATE,'request-action-'+id);if(fs.existsSync(f)){const title=fs.readFileSync(f,'utf8').trim();fs.unlinkSync(f);
  log('action',await call('requestAction',{title,description:'Fixture proposal: '+title}))}await new Promise(r=>setTimeout(r,300))}}
(async()=>{
 try{
  if(agent==='claude')hook('SessionStart',{source:'startup'});else hook('PreToolUse',{tool_name:'Read'});
  if(role==='companion'){log('tools',(await rpc('tools/list',{})).tools.map(t=>t.name).sort());
   log('sent',await call('sendMessageToArchitect',{message:'hello from '+path.basename(process.cwd())}));actions()}
  if(agent==='claude')claudeLoop();else codexLoop();
 }catch(e){log('error',String(e&&e.stack||e))}
})();
setInterval(()=>{},1<<30);
'''

ntfy_posts = []
class Ntfy(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        ntfy_posts.append(json.loads(self.rfile.read(int(self.headers['Content-Length']))))
        self.send_response(200); self.end_headers(); self.wfile.write(b'{}')
    def log_message(self, *a): pass

container = None; daemon = None
with tempfile.TemporaryDirectory(prefix='hiveryn-companion-smoke-') as tmpdir:
    tmp = pathlib.Path(tmpdir)
    home = tmp/'hiveryn-home'; userhome = tmp/'user-home'; workspace = tmp/'workspace'; bindir = tmp/'bin'; state = tmp/'state'; localrepo = tmp/'local-repo'
    for d in (home, userhome, workspace, bindir, state, localrepo): d.mkdir()
    daemonport, sshport = free_port(), free_port()
    ntfy = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Ntfy); threading.Thread(target=ntfy.serve_forever, daemon=True).start()
    def api(path, body=None, method=None, ok=True):
        req = urllib.request.Request(f'http://127.0.0.1:{daemonport}/api' + path, data=None if body is None else json.dumps(body).encode(), headers={'Content-Type': 'application/json'}, method=method)
        try:
            with urllib.request.urlopen(req, timeout=60) as response: result = json.load(response)
        except urllib.error.HTTPError as e:
            result = json.loads(e.read())
        if ok and result.get('error'): raise AssertionError(result['error'])
        return result['data'] if ok else result
    def remote(script): return run('/usr/bin/ssh', '-F', str(tmp/'ssh_config'), 'fixture', script)
    def remote_file(name):
        try: return remote(f'cat /home/worker/{name} 2>/dev/null || true')
        except subprocess.CalledProcessError: return ''
    def local_file(name):
        p = state/name
        return p.read_text() if p.exists() else ''
    env = dict(os.environ, HIVERYN_HOME=str(home), HOME=str(userhome), PATH=str(bindir) + os.pathsep + os.environ['PATH'], TZ='UTC')
    for key in ('CLAUDE_CONFIG_DIR', 'CODEX_HOME', 'SSH_AUTH_SOCK'): env.pop(key, None)
    def start():
        global daemon
        log = open(tmp/'daemon.log', 'a')
        daemon = subprocess.Popen([str(tmp/'hiverynd'), 'serve'], env=env, stdout=log, stderr=log)
        wait(lambda: api('/health'), what='daemon health')
    def stop():
        global daemon
        daemon.terminate(); daemon.wait(timeout=30); daemon = None
    class ArchitectMCP:
        """The architect's own MCP server (hiverynd mcp, architect role), over stdio."""
        def __init__(self, session_id):
            self.p = subprocess.Popen([str(tmp/'hiverynd'), 'mcp'], env=dict(env, HIVERYN_DAEMON_URL=f'http://127.0.0.1:{daemonport}', HIVERYN_ARCHITECT_KEY='fixture', HIVERYN_SESSION_TYPE='architect', HIVERYN_SESSION_ID=session_id), stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
            self.n = 0
            self.rpc('initialize', {'protocolVersion': '2025-06-18', 'capabilities': {}, 'clientInfo': {'name': 'smoke', 'version': '0'}})
            self.p.stdin.write(json.dumps({'jsonrpc': '2.0', 'method': 'notifications/initialized'}) + '\n'); self.p.stdin.flush()
        def rpc(self, method, params):
            self.n += 1
            self.p.stdin.write(json.dumps({'jsonrpc': '2.0', 'id': self.n, 'method': method, 'params': params}) + '\n'); self.p.stdin.flush()
            while True:
                m = json.loads(self.p.stdout.readline())
                if m.get('id') == self.n: return m['result']
        def call(self, name, args):
            r = self.rpc('tools/call', {'name': name, 'arguments': args})
            return ('ERROR', r['content'][0]['text']) if r.get('isError') else r['structuredContent']
        def close(self): self.p.kill(); self.p.wait()
    try:
        run('docker', 'build', '-q', '-t', 'hiveryn-remote-ticket-fixture', str(ROOT/'internal/remoteexec/testdata'))
        run('ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(tmp/'key'))
        container = run('docker', 'run', '-d', '--rm', '-p', f'127.0.0.1:{sshport}:22', 'hiveryn-remote-ticket-fixture')
        run('docker', 'cp', str(tmp/'key.pub'), container + ':/home/worker/.ssh/authorized_keys')
        run('docker', 'exec', container, 'sh', '-c', 'chown -R worker:worker /home/worker/.ssh; chmod 700 /home/worker/.ssh; chmod 600 /home/worker/.ssh/authorized_keys')
        hostkey = run('docker', 'exec', container, 'cat', '/etc/ssh/ssh_host_ed25519_key.pub').split()
        (tmp/'known_hosts').write_text(f'[127.0.0.1]:{sshport} {hostkey[0]} {hostkey[1]}\n')
        (tmp/'ssh_config').write_text(f'Host fixture\n HostName 127.0.0.1\n Port {sshport}\n User worker\n IdentityFile {tmp}/key\n UserKnownHostsFile {tmp}/known_hosts\n StrictHostKeyChecking yes\n IdentitiesOnly yes\n')
        # ssh_down makes the daemon's NEW SSH connections fail (established ones, such as the
        # attachment carrying the reverse tunnel, continue): an unreachable machine for termination.
        (bindir/'ssh').write_text('#!/bin/sh\nif [ -e ' + shlex.quote(str(tmp/'ssh_down')) + ' ]; then echo "ssh: connect to host fixture: Connection refused" >&2; exit 255; fi\nexec /usr/bin/ssh -F ' + shlex.quote(str(tmp/'ssh_config')) + ' "$@"\n'); (bindir/'ssh').chmod(0o700)
        wait(lambda: remote('true') == '', what='sshd')
        (tmp/'fixture').write_text(FIXTURE)
        for name in ('claude', 'codex'):
            run('docker', 'cp', str(tmp/'fixture'), container + f':/usr/local/bin/{name}')
            (bindir/name).write_text(FIXTURE); (bindir/name).chmod(0o755)
        run('docker', 'exec', container, 'chmod', '+x', '/usr/local/bin/claude', '/usr/local/bin/codex')
        remote('for r in repo1 repo2; do mkdir -p /home/worker/$r && git -C /home/worker/$r init -q; done; printf "# rc responsibilities\\nBrowse.\\n" > /home/worker/repo1/AGENTS.md')
        run('git', 'init', '-q', str(localrepo))
        (home/'config.yaml').write_text(f'port: {daemonport}\nbind_address: 127.0.0.1\nshell: /bin/sh\nnotifications:\n  ntfy:\n    server: http://127.0.0.1:{ntfy.server_address[1]}\n    topic: smoke\n')
        (home/'machines.yaml').write_text('box:\n  ssh: fixture\n')
        (home/'architects.yaml').write_text(f'fixture: {workspace}\n')
        (workspace/'hiveryn.yaml').write_text(
            'name: Fixture\nrepos:\n  r1:\n    path: /home/worker/repo1\n    machine: box\n  r2:\n    path: /home/worker/repo2\n    machine: box\n'
            f'  lc: {localrepo}\ncompanions:\n  rc:\n    repo: r1\n    variant: box-claude\n  rx:\n    repo: r2\n    variant: box-codex\n  lc:\n    repo: lc\n    variant: local-claude\n')
        (workspace/'OVERVIEW.md').write_text('---\nupdated_at: 2026-10-30T00:00:00Z\n---\nFixture.\n')
        (home/'variants.yaml').write_text(
            f'arch:\n  agent: codex\n  env:\n    CODEX_HOME: {tmp}/arch-codex\n    FIXTURE_AGENT: codex\n    FIXTURE_ROLE: architect\n    FIXTURE_STATE: {state}\n'
            f'local-claude:\n  agent: claude\n  env:\n    CLAUDE_CONFIG_DIR: {tmp}/lc-claude\n    FIXTURE_AGENT: claude\n    FIXTURE_STATE: {state}\n'
            'box-claude:\n  agent: claude\n  machine: box\n  env:\n    FIXTURE_AGENT: claude\n'
            'box-codex:\n  agent: codex\n  machine: box\n  env:\n    FIXTURE_AGENT: codex\n')
        run('go', 'build', '-race', '-o', str(tmp/'hiverynd'), './cmd/hiverynd', cwd=ROOT)
        start()

        arch = api('/sessions', {'session_type': 'architect', 'architect_key': 'fixture'})['id']
        api(f'/sessions/{arch}/runs', {'profile_name': 'arch'})
        def companions(): return {c['name']: c for c in api('/architects/fixture/companions')['companions']}
        cs = wait(lambda: (lambda c: c if len(c) == 3 and all(x['status'] == 'running' for x in c.values()) else None)(companions()), timeout=180, what='companions running')
        sid = {name: c['session_id'] for name, c in cs.items()}
        assert cs['rc']['machine'] == 'box' and cs['rx']['machine'] == 'box' and not cs['lc'].get('machine'), cs
        assert cs['rc']['agent'] == 'claude' and cs['rx']['agent'] == 'codex' and cs['lc']['agent'] == 'claude', cs
        for name in ('rc', 'rx'):
            wait(lambda: companions()[name].get('connection') == 'connected', what=f'{name} attached')
        status = api('/architects/status')['architects'][0]
        assert {c['name'] for c in status['companions']} == {'rc', 'rx', 'lc'} and status['sessions'] == [], status
        step('local Claude + remote Claude/Codex companions start with the architect session (status, location, attachment)')

        read = lambda name, f: (remote_file if name != 'lc' else local_file)(f'{f}-{sid[name]}')
        want_tools = ['completeCompanionShutdown', 'replyToArchitectMessage', 'requestAction', 'sendMessageToArchitect']
        for name in sid:
            tools = wait(lambda: read(name, 'tools'), what=f'{name} tools')
            assert json.loads(tools.splitlines()[0]) == want_tools, (name, tools)
            err = read(name, 'error')
            assert not err, (name, err)
        assert remote('cat /home/worker/launches').split() .count(sid['rc']) == 1
        step('each companion sees exactly the four companion tools (gateway for remote, stdio for local)')

        startup = wait(lambda: [m for m in api('/architects/fixture/messages')['messages'] if m['kind'] == 'companion_startup'], what='startup report')
        assert len(startup) == 1 and startup[0]['recipient'] == 'session:' + arch and all(f'- {n}: started' in startup[0]['body'] for n in sid), startup
        arch_inbox = lambda: local_file(f'inbox-{arch}')
        wait(lambda: 'Companion startup outcomes' in arch_inbox(), what='startup report delivered to the architect')
        for name in sid:
            wait(lambda: f'From companion {name} ' in arch_inbox(), timeout=120, what=f'{name} hello delivered to the architect')
        step('startup outcomes and companion→architect messages delivered to the architect (local Codex queue)')

        mcp = ArchitectMCP(arch)
        names = {t['name'] for t in mcp.rpc('tools/list', {})['tools']}
        assert {'sendMessageToCompanion', 'replyToCompanionMessage', 'requestCompanionShutdown', 'readContext', 'listContext'} <= names and 'listCompanions' not in names, names
        agents = mcp.call('readContext', {'reference': 'r1:AGENTS.md'})
        assert agents['status'] == 'ok' and 'rc responsibilities' in agents['content'] and agents['location'] == 'remote', agents
        step('architect tools present; remote companion AGENTS.md readable through readContext')

        for name in sid:
            sent = mcp.call('sendMessageToCompanion', {'companion': name, 'message': f'assignment for {name}'})
            assert sent['status'] == 'accepted', sent
            wait(lambda: f'assignment for {name}' in read(name, 'inbox'), timeout=90, what=f'{name} received the assignment')
            wait(lambda: sent['message_id'] in read(name, 'replied'), what=f'{name} replied')
            wait(lambda: f'Reply from companion {name} to your message {sent["message_id"]}' in arch_inbox(), what=f'{name} reply delivered')
        hello = [m for m in api('/architects/fixture/messages')['messages'] if m['kind'] == 'companion_message' and 'hello from' in m['body'] and m['payload']['companion'] == 'rx'][0]
        answer = mcp.call('replyToCompanionMessage', {'message_id': hello['id'], 'message': 'noted, rx'})
        assert answer['in_reply_to'] == hello['id'], answer
        wait(lambda: 'noted, rx' in read('rx', 'inbox'), what='architect reply delivered to rx')
        wrong = mcp.call('replyToCompanionMessage', {'message_id': answer['message_id'], 'message': 'x'})
        assert wrong[0] == 'ERROR' and 'NOT_FOUND' in wrong[1], wrong
        step('architect→companion assignments and replies delivered: remote Claude (waiter via tunnel), remote Codex (app-server over SSH), local Claude; reply routing and wrong-message rejection')

        decisions = {'rc': {'decision': 'accept'}, 'rx': {'decision': 'reply', 'message': 'Use project XYZ.'}, 'lc': {'decision': 'deny'}}
        expect = {'rc': {'outcome': 'accepted'}, 'rx': {'outcome': 'replied', 'message': 'Use project XYZ.'}, 'lc': {'outcome': 'denied'}}
        for name in sid:
            if name == 'lc': (state/f'request-action-{sid[name]}').write_text(f'Push branch for {name}')
            else: remote(f'echo "Push branch for {name}" > /home/worker/request-action-{sid[name]}')
            pending = wait(lambda: api(f'/sessions/{sid[name]}/actions')['actions'], what=f'{name} action pending')
            assert pending[0]['title'] == f'Push branch for {name}' and pending[0]['origin']['companion'] == name, pending
            assert companions()[name]['pending_actions'][0]['id'] == pending[0]['id']
            api(f'/sessions/{sid[name]}/actions/{pending[0]["id"]}/decision', decisions[name])
            result = wait(lambda: read(name, 'action'), what=f'{name} action result')
            assert json.loads(result) == expect[name], (name, result)
        assert len(ntfy_posts) == 3 and all('action request' in p['title'] for p in ntfy_posts), ntfy_posts
        step('requestAction: phone alert, pending in the desktop listing, accept/reply/deny reach the companion')

        blocked = api(f'/sessions/{arch}/conclude', {'body': 'x'}, ok=False)
        assert 'companion(s) not stopped' in blocked['error']['message'], blocked
        step('architect conclusion blocked while companions run')

        stop()
        start()
        cs = wait(lambda: (lambda c: c if all(x['status'] == 'running' for x in c.values()) and c['rc'].get('connection') == 'connected' and c['rx'].get('connection') == 'connected' else None)(companions()), timeout=90, what='companions after restart')
        assert {n: c['session_id'] for n, c in cs.items()} == sid
        launches = remote('cat /home/worker/launches').split()
        assert launches.count(sid['rc']) == 1 and launches.count(sid['rx']) == 1, launches
        sent = mcp.call('sendMessageToCompanion', {'companion': 'rc', 'message': 'after restart'})
        wait(lambda: 'after restart' in read('rc', 'inbox'), timeout=90, what='rc message after restart')
        original = companions()['rc']['main_terminal_id']
        remote(f"tmux -L hiveryn-{sid['rc']} detach-client -s worker")
        wait(lambda: companions()['rc']['main_terminal_id'] != original and companions()['rc'].get('connection') == 'connected', what='rc reattached')
        assert remote('cat /home/worker/launches').split().count(sid['rc']) == 1
        step('daemon restart and SSH disconnect: same companions, reattached, no duplicate remote launch, messaging resumes')

        request = {name: str(uuid.uuid4()) for name in sid}
        remote(f"touch /home/worker/hold-{sid['rx']}")
        for name in sid:
            ok = mcp.call('requestCompanionShutdown', {'companion': name, 'request_id': request[name]})
            assert ok == {'request_id': request[name], 'status': 'accepted'}, ok
        assert mcp.call('requestCompanionShutdown', {'companion': 'rc', 'request_id': request['rc']}) == {'request_id': request['rc'], 'status': 'accepted'}
        conflict = mcp.call('requestCompanionShutdown', {'companion': 'rc', 'request_id': str(uuid.uuid4())})
        assert conflict[0] == 'ERROR' and 'already shutting down' in conflict[1], conflict
        reused = mcp.call('requestCompanionShutdown', {'companion': 'lc', 'request_id': request['rc']})
        assert reused[0] == 'ERROR' and 'different shutdown target' in reused[1], reused
        for name in ('rc', 'lc'):
            wait(lambda: companions()[name]['status'] == 'stopped', timeout=90, what=f'{name} stopped')
            assert request[name] in read(name, 'completed'), name
            wait(lambda: f'Companion {name} shutdown (request_id {request[name]}): stopped; termination confirmed' in arch_inbox(), what=f'{name} outcome delivered')
        assert remote(f"tmux -L hiveryn-{sid['rc']} has-session 2>/dev/null || echo gone") == 'gone'
        assert remote(f"test ! -d /home/worker/.local/state/hiveryn-workers/{sid['rc']} && echo cleaned") == 'cleaned'
        assert api(f"/sessions/{sid['lc']}", ok=False)['error']['code'] == 'NOT_FOUND'
        step('cooperative shutdown with request_id binding: idempotent repeat, conflicting/reused ids refused, confirmed termination, outcome with handoff to the architect')

        wait(lambda: request['rx'] in read('rx', 'inbox'), what='rx received its shutdown instruction')
        (tmp/'ssh_down').write_text('')
        run('docker', 'exec', container, 'rm', f"/home/worker/hold-{sid['rx']}")
        wait(lambda: companions()['rx']['status'] == 'stop_failed', timeout=120, what='rx stop_failed')
        blocked = api(f'/sessions/{arch}/conclude', {'body': 'x'}, ok=False)
        assert 'rx (stop_failed)' in blocked['error']['message'], blocked
        wait(lambda: 'NOT confirmed' in arch_inbox(), what='unconfirmed termination reported')
        (tmp/'ssh_down').unlink()
        api(f"/sessions/{sid['rx']}/discard", {})
        assert companions()['rx']['status'] == 'stopped'
        assert remote(f"tmux -L hiveryn-{sid['rx']} has-session 2>/dev/null || echo gone") == 'gone'
        step('unreachable machine at termination: stop_failed, reported, conclusion blocked; user discard confirms termination')

        mcp.close()
        api(f'/sessions/{arch}/conclude', {'body': 'Fixture conclusion.'})
        assert api('/architects/fixture/companions')['companions'] == []
        step('architect concludes once every companion stopped')
        print('companion smoke: ALL PASS (fixture providers, not real Claude/Codex)', flush=True)
    except Exception:
        log = tmp/'daemon.log'
        if log.exists(): print(log.read_text()[-6000:], file=sys.stderr)
        try:
            for m in api('/architects/fixture/messages')['messages']:
                print('MESSAGE', m['kind'], m['recipient'], m['state'], m.get('condition', ''), m.get('last_error', ''), file=sys.stderr)
        except Exception as e: print('messages unavailable:', e, file=sys.stderr)
        raise
    finally:
        if daemon: daemon.terminate(); daemon.wait(timeout=30)
        if container: subprocess.run(['docker', 'stop', container], capture_output=True)
        ntfy.shutdown()
