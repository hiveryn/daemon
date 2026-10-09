#!/usr/bin/env python3
"""Disposable real SSH/tmux + daemon smoke. Uses fixture CLIs, not model providers.
Requires Docker, Go, Python 3 with websocket-client, OpenSSH. Never reads live Hiveryn or SSH config.
"""
import json, os, pathlib, shlex, socket, sqlite3, subprocess, tempfile, threading, time, urllib.request
import websocket

ROOT = pathlib.Path(__file__).resolve().parents[1]
def run(*args, **kw):
    return subprocess.run(args, check=True, text=True, capture_output=True, **kw).stdout.strip()
def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1',0)); return s.getsockname()[1]
def wait(fn, timeout=40):
    end=time.monotonic()+timeout; last=None
    while time.monotonic()<end:
        try:
            value=fn()
            if value: return value
        except Exception as e: last=e
        time.sleep(.25)
    raise AssertionError(f'timed out: {last}')

provider = r'''#!/usr/bin/env node
const fs=require('fs');
const id=process.env.AGENTRUNTIME_SESSION_ID;
const base=process.env.AGENTRUNTIME_HOOK_ENDPOINT.split('/hooks/')[0];
const agent=process.env.SMOKE_AGENT;
if(agent==='opencode'){
 const config=JSON.parse(process.env.OPENCODE_CONFIG_CONTENT);
 if(config.mcp['hiveryn-daemon'].headers.Authorization!=='Bearer {env:HIVERYN_WORKER_TOKEN}' || config.mcp['hiveryn-daemon'].oauth!==false)throw Error('invalid OpenCode bearer config');
} else if(agent==='claude'){
 const config=JSON.parse(fs.readFileSync(process.argv[process.argv.indexOf('--mcp-config')+1]));
 if(config.mcpServers['hiveryn-daemon'].headers.Authorization!=='Bearer ${HIVERYN_WORKER_TOKEN}')throw Error('invalid Claude bearer config');
} else if(!process.argv.some(a=>a.includes('bearer_token_env_var')&&a.includes('HIVERYN_WORKER_TOKEN')))throw Error('invalid Codex bearer config');
const headers={'Authorization':'Bearer '+process.env.HIVERYN_WORKER_TOKEN,'Content-Type':'application/json','Accept':'application/json, text/event-stream'};
fs.appendFileSync('/home/worker/launches',id+'\n');
async function rpc(method,params){
 const r=await fetch(base+'/mcp',{method:'POST',headers,body:JSON.stringify({jsonrpc:'2.0',id:1,method,params})});
 const body=await r.text();if(!r.ok)throw Error(r.status+' '+body);
 const data=body.startsWith('event:')||body.startsWith('data:')?JSON.parse(body.split('\n').find(s=>s.startsWith('data:')).slice(5)):JSON.parse(body);
 if(data.error)throw Error(JSON.stringify(data.error));return data.result;
}
(async()=>{
 const tools=await rpc('tools/list',{});
 const ticket=await rpc('tools/call',{name:'readTicket',arguments:{id:process.env.SMOKE_TICKET_ID}});
 const created=await rpc('tools/call',{name:'createWorkTicket',arguments:{title:agent+' followup fixture',repo:'remote',body:'Fixture auto-approval'}});
 if(created.isError)throw Error(JSON.stringify(created));
 const denied=await fetch(base+'/mcp',{method:'POST',headers:{'Content-Type':'application/json'},body:'{}'});
 const api=await fetch(base+'/api/health',{headers});
 if(denied.status!==401 || api.status!==404)throw Error('gateway isolation failed');
 if(agent==='opencode'){
  await fetch(process.env.AGENTRUNTIME_HOOK_ENDPOINT+'/opencode',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({hook_event_name:'tool.execute.before',session_id:'native-'+id,agentruntime_session_id:id,payload:{tool:'read'}})});
 }else{
  const root=agent==='claude'?'/home/worker/.claude/settings.json':'/home/worker/.codex/hooks.json';
  const config=JSON.parse(fs.readFileSync(root));
  const command=config.hooks.PreToolUse[0].hooks[0].command;
  require('child_process').execSync(command,{input:JSON.stringify({hook_event_name:'PreToolUse',session_id:'native-'+id,tool_name:'Read',cwd:process.cwd()}),env:process.env});
 }
 fs.writeFileSync('/home/worker/result-'+id,JSON.stringify({tools:tools.tools.map(t=>t.name),ticket,created}));
 console.log('FIXTURE_READY');
})().catch(e=>{fs.writeFileSync('/home/worker/error-'+id,String(e));console.error(e)});
setInterval(async()=>{
 try {await rpc('tools/list',{});fs.writeFileSync('/home/worker/ping-'+id,String(Date.now()));}catch{}
 if(fs.existsSync('/home/worker/conclude-'+id)){
  fs.unlinkSync('/home/worker/conclude-'+id);
  const sha=require('child_process').execSync('git rev-parse HEAD').toString().trim();
  try{await rpc('tools/call',{name:'concludeTicketSession',arguments:{summary:'Remote fixture complete',outcome:'completed',implementation:'SSH fixture',verification:'Remote fixture checks passed',commits:[{repo:'remote',sha}]}});}catch{}
 }
},1000);
'''

container=None; daemon=None
with tempfile.TemporaryDirectory(prefix='hiveryn-remote-smoke-') as tmp:
    tmp=pathlib.Path(tmp); home=tmp/'home'; home.mkdir(); workspace=tmp/'workspace'; workspace.mkdir(); bindir=tmp/'bin';bindir.mkdir()
    daemonport=port();sshport=port()
    def api(path,body=None):
        req=urllib.request.Request(f'http://127.0.0.1:{daemonport}/api'+path,data=None if body is None else json.dumps(body).encode(),headers={'Content-Type':'application/json'})
        try:
            with urllib.request.urlopen(req,timeout=50) as response: result=json.load(response)
        except urllib.error.HTTPError as e: raise AssertionError(e.read().decode()) from e
        if result.get('error'):raise AssertionError(result)
        return result['data']
    def remote(script):return run('/usr/bin/ssh','-F',str(tmp/'ssh_config'),'fixture',script)
    def start():
        global daemon
        log=open(tmp/'daemon.log','a')
        daemon=subprocess.Popen([str(tmp/'hiverynd'),'serve'],env=env,stdout=log,stderr=log)
        wait(lambda:api('/health'))
    try:
        run('docker','build','-q','-t','hiveryn-remote-ticket-fixture',str(ROOT/'internal/remoteexec/testdata'))
        run('ssh-keygen','-q','-t','ed25519','-N','','-f',str(tmp/'key'))
        container=run('docker','run','-d','--rm','-p',f'127.0.0.1:{sshport}:22','hiveryn-remote-ticket-fixture')
        run('docker','cp',str(tmp/'key.pub'),container+':/home/worker/.ssh/authorized_keys')
        run('docker','exec',container,'sh','-c','chown -R worker:worker /home/worker/.ssh; chmod 700 /home/worker/.ssh; chmod 600 /home/worker/.ssh/authorized_keys')
        hostkey=run('docker','exec',container,'cat','/etc/ssh/ssh_host_ed25519_key.pub').split()
        (tmp/'known_hosts').write_text(f'[127.0.0.1]:{sshport} {hostkey[0]} {hostkey[1]}\n')
        (tmp/'ssh_config').write_text(f'Host fixture\n HostName 127.0.0.1\n Port {sshport}\n User worker\n IdentityFile {tmp}/key\n UserKnownHostsFile {tmp}/known_hosts\n StrictHostKeyChecking yes\n IdentitiesOnly yes\n')
        # ssh_delay injects per-connection latency for the launch-lifetime phase.
        (tmp/'ssh_delay').write_text('0')
        # ssh_calls counts the daemon's SSH connections (one line each).
        (bindir/'ssh').write_text('#!/bin/sh\necho x >> '+shlex.quote(str(tmp/'ssh_calls'))+'\nsleep "$(cat '+shlex.quote(str(tmp/'ssh_delay'))+')"\nexec /usr/bin/ssh -F '+shlex.quote(str(tmp/'ssh_config'))+' "$@"\n');(bindir/'ssh').chmod(0o700)
        wait(lambda:remote('true')=='')
        (tmp/'provider').write_text(provider)
        run('docker','cp',str(tmp/'provider'),container+':/usr/local/bin/claude')
        run('docker','exec',container,'sh','-c','chmod +x /usr/local/bin/claude; ln -s claude /usr/local/bin/codex; ln -s claude /usr/local/bin/opencode')
        remote('git -C /home/worker/repo init -q; git -C /home/worker/repo config user.email fixture@example.test; git -C /home/worker/repo config user.name Fixture; echo before >/home/worker/repo/file; git -C /home/worker/repo add file; git -C /home/worker/repo commit -qm initial; echo after >>/home/worker/repo/file')
        (home/'config.yaml').write_text(f'port: {daemonport}\nbind_address: 127.0.0.1\nintent_wait_timeout: 1\nshell: /bin/sh\n')
        (home/'machines.yaml').write_text('box:\n  ssh: fixture\n')
        (home/'architects.yaml').write_text(f'fixture: {workspace}\n')
        (workspace/'hiveryn.yaml').write_text('name: Fixture\nrepos:\n  remote:\n    path: /home/worker/repo\n    machine: box\n')
        for name in ['PROJECT_OVERVIEW.md','PROJECT_STATE.md']:(workspace/name).write_text('---\nlastUpdatedAt: 2026-10-09T00:00:00Z\n---\nFixture project context.\n')
        env=dict(os.environ,HIVERYN_HOME=str(home),PATH=str(bindir)+os.pathsep+os.environ['PATH'],TZ='UTC')
        (home/'variants.yaml').write_text('fixture:\n  agent: claude\n  machine: box\n  yolo: true\n')
        run('go','build','-race','-o',str(tmp/'hiverynd'),'./cmd/hiverynd',cwd=ROOT)
        start()
        for agent in ['claude','codex','opencode']:
            ticket=api('/architects/fixture/tickets',{'title':f'{agent} remote smoke','repo':'remote','body':'Fixture'})
            (home/'variants.yaml').write_text(f'fixture:\n  agent: {agent}\n  machine: box\n  yolo: true\n  env:\n    SMOKE_TICKET_ID: {ticket["id"]}\n    SMOKE_AGENT: {agent}\n')
            session=api('/sessions',{'session_type':'ticket','architect_key':'fixture','ticket_id':ticket['id']})
            sid=session['id']
            api(f'/sessions/{sid}/runs',{'profile_name':'fixture'})
            result=json.loads(wait(lambda:remote(f'cat /home/worker/result-{sid}')))
            assert 'readTicket' in result['tools'] and 'spawnTicketWorker' not in result['tools'],result
            assert not result['ticket'].get('isError'),result
            assert api('/architects/fixture/repos/remote/diff')['files']
            sha=remote('git -C /home/worker/repo rev-parse HEAD')
            assert api(f'/architects/fixture/repos/remote/commits/{sha}/diff')['files']
            current=api(f'/sessions/{sid}')
            assert current['connection']=='connected',current
            wait(lambda:api(f'/sessions/{sid}')['current_run'].get('native_id')=='native-'+sid)
            assert 'remote_token' not in current and 'ssh' not in current
            with sqlite3.connect(home/'daemon.db') as db:token=db.execute('select remote_token from sessions where id=?',(sid,)).fetchone()[0]
            for log in (home/'logs').glob('*.jsonl'):assert token not in log.read_text(), 'credential leaked to logs'
            aux=api(f'/sessions/{sid}/terminals',{'workdir_id':'session-primary'})
            auxid=aux['terminal_id']
            ws=websocket.create_connection(f'ws://127.0.0.1:{daemonport}/ws/session/{sid}/terminal/{auxid}?cols=100&rows=30',timeout=10)
            ws.send('pwd > /home/worker/terminal-cwd; stty size > /home/worker/terminal-size\n')
            wait(lambda:remote('cat /home/worker/terminal-cwd')=='/home/worker/repo')
            wait(lambda:remote('cat /home/worker/terminal-size')=='30 100')
            ws.close()
            if agent=='claude':
                run('docker','pause',container)
                try:
                    try:api(f'/sessions/{sid}/discard',{})
                    except AssertionError as e:assert 'termination not confirmed' in str(e),e
                    else:raise AssertionError('unreachable remote was reported stopped')
                    assert api(f'/sessions/{sid}')['current_run']['status']=='running'
                    assert api(f'/architects/fixture/tickets/{ticket["id"]}')['status']=='progress'
                finally:run('docker','unpause',container)
            original=current['current_run']['main_terminal_id']
            # Kill only fixture SSH clients of this worker from inside its owned
            # server; provider remains in tmux. No live user process is targeted.
            remote(f"tmux -L hiveryn-{sid} detach-client -s worker")
            wait(lambda:api(f'/sessions/{sid}')['current_run']['main_terminal_id']!=original)
            assert remote(f"grep -c {shlex.quote(sid)} /home/worker/launches")=='1'
            ping=remote(f'cat /home/worker/ping-{sid}')
            daemon.terminate();daemon.wait(timeout=15);daemon=None
            assert remote(f"tmux -L hiveryn-{sid} has-session -t worker") == ''
            start()
            wait(lambda:api(f'/sessions/{sid}').get('connection')=='connected')
            assert remote(f"grep -c {shlex.quote(sid)} /home/worker/launches")=='1'
            wait(lambda:any(tab.get('id')==auxid for tab in api(f'/sessions/{sid}/tabs')))
            ws=websocket.create_connection(f'ws://127.0.0.1:{daemonport}/ws/session/{sid}/terminal/{auxid}?cols=90&rows=25',timeout=10)
            ws.send('echo restored > /home/worker/terminal-restored\n');ws.close()
            wait(lambda:remote('cat /home/worker/terminal-restored')=='restored')
            wait(lambda:remote(f'cat /home/worker/ping-{sid}')!=ping)
            # Exercise remote commit validation through approved MCP conclusion.
            if agent=='opencode':
                remote(f'touch /home/worker/conclude-{sid}')
                wait(lambda:api(f'/architects/fixture/tickets/{ticket["id"]}')["status"]=='done')
            else:
                if agent=='codex':
                    remote(f'tmux -L hiveryn-{sid} kill-server')
                    wait(lambda:api(f'/sessions/{sid}').get('connection')=='missing')
                    assert api(f'/sessions/{sid}')['current_run']['status']=='running'
                api(f'/sessions/{sid}/discard',{})
            assert remote(f'test ! -d /home/worker/.local/state/hiveryn-workers/{sid} && echo cleaned')=='cleaned'
            print(f'{agent}: authenticated tools/mutation/hooks, remote diffs, terminal input/resize/restore, detach/reattach, daemon restart, one launch and cleanup PASS',flush=True)
        # Launch lifetime: with slow SSH, preparing a worker takes well over the
        # desktop's former 5 s request bound.
        def post(path,body,timeout):
            req=urllib.request.Request(f'http://127.0.0.1:{daemonport}/api'+path,data=json.dumps(body).encode(),headers={'Content-Type':'application/json'})
            try:
                with urllib.request.urlopen(req,timeout=timeout) as response:return response.status,json.load(response)
            except urllib.error.HTTPError as e:return e.code,json.loads(e.read())
        def fresh(title):
            ticket=api('/architects/fixture/tickets',{'title':title,'repo':'remote','body':'Fixture'})
            (home/'variants.yaml').write_text(f'fixture:\n  agent: claude\n  machine: box\n  yolo: true\n  env:\n    SMOKE_TICKET_ID: {ticket["id"]}\n    SMOKE_AGENT: claude\n')
            return ticket,api('/sessions',{'session_type':'ticket','architect_key':'fixture','ticket_id':ticket['id']})['id']
        def launched(sid):return remote(f"grep -c {shlex.quote(sid)} /home/worker/launches || true")
        (tmp/'ssh_delay').write_text('0.7')
        # A requester that gives up at 5 s no longer interrupts the launch.
        ticket,sid=fresh('slow launch, requester leaves')
        started=time.monotonic()
        try:post(f'/sessions/{sid}/runs',{'profile_name':'fixture'},5)
        except (TimeoutError,socket.timeout,urllib.error.URLError):pass
        else:raise AssertionError('launch answered within 5 s; latency injection ineffective')
        wait(lambda:api(f'/sessions/{sid}').get('connection')=='connected',timeout=120)
        assert time.monotonic()-started>5
        wait(lambda:remote(f'cat /home/worker/result-{sid}'),timeout=60)
        assert launched(sid)=='1'
        assert api(f'/architects/fixture/tickets/{ticket["id"]}')['status']=='progress'
        assert 'requester stopped waiting' in (home/'logs/daemon.jsonl').read_text()
        api(f'/sessions/{sid}/discard',{})
        # A waiting requester gets the result after >5 s; a concurrent launch
        # of the same session is a conflict, not a second worker.
        ticket,sid=fresh('slow launch, requester waits')
        outcome={}
        def first():
            began=time.monotonic();outcome['result']=post(f'/sessions/{sid}/runs',{'profile_name':'fixture'},150);outcome['seconds']=time.monotonic()-began
        thread=threading.Thread(target=first);thread.start();time.sleep(1.5)
        status,body=post(f'/sessions/{sid}/runs',{'profile_name':'fixture'},150)
        assert status==409 and 'already in progress' in body['error']['message'],(status,body)
        thread.join()
        assert outcome['result'][0]==201 or outcome['result'][0]==200,outcome
        assert outcome['seconds']>5,outcome
        wait(lambda:remote(f'cat /home/worker/result-{sid}'),timeout=60)
        assert launched(sid)=='1'
        api(f'/sessions/{sid}/discard',{})
        (tmp/'ssh_delay').write_text('0')
        # A launch failing mid-preparation removes its never-started worker, so
        # the retry of the same session is clean and launches exactly once.
        ticket,sid=fresh('failed preparation then retry')
        remote('mv /home/worker/.claude /home/worker/.claude-saved && touch /home/worker/.claude')
        try:
            status,body=post(f'/sessions/{sid}/runs',{'profile_name':'fixture'},150)
            assert status==500 and 'prepare remote worker on box' in body['error']['message'],(status,body)
        finally:remote('rm /home/worker/.claude && mv /home/worker/.claude-saved /home/worker/.claude')
        assert remote(f'test ! -d /home/worker/.local/state/hiveryn-workers/{sid} && echo cleaned')=='cleaned'
        assert api(f'/sessions/{sid}')['current_run']['status']=='failed'
        assert api(f'/architects/fixture/tickets/{ticket["id"]}')['status']=='backlog'
        status,body=post(f'/sessions/{sid}/runs',{'profile_name':'fixture'},150)
        assert status in (200,201),(status,body)
        wait(lambda:remote(f'cat /home/worker/result-{sid}'),timeout=60)
        assert launched(sid)=='1'
        api(f'/sessions/{sid}/discard',{})
        # A managed server this launch did not create is reported, never killed.
        ticket,sid=fresh('pre-existing owned server')
        remote(f'tmux -L hiveryn-{sid} new-session -d -s worker sleep 600')
        status,body=post(f'/sessions/{sid}/runs',{'profile_name':'fixture'},150)
        assert status==500 and 'discard the session' in body['error']['message'],(status,body)
        assert remote(f'tmux -L hiveryn-{sid} has-session -t worker && echo alive')=='alive'
        assert launched(sid)=='0'
        api(f'/sessions/{sid}/discard',{})
        assert remote(f'tmux -L hiveryn-{sid} has-session 2>/dev/null || echo gone')=='gone'
        print('launch lifetime: >5 s launch survives requester timeout, waiting requester succeeds, concurrent launch 409, failed prep cleaned and retried once, foreign server untouched PASS',flush=True)
        # Diffs and repository terminals with slow SSH: both take well over the
        # desktop's former 5 s request bound and must still succeed.
        ticket,sid=fresh('slow diffs and terminals')
        api(f'/sessions/{sid}/runs',{'profile_name':'fixture'})
        wait(lambda:remote(f'cat /home/worker/result-{sid}'),timeout=60)
        remote('cd /home/worker/repo && mkdir -p "new dir" && for i in 1 2 3 4 5 6 7 8 9 10; do echo "line $i" > "new dir/untracked $i.txt"; done')
        def ssh_calls():
            try:return len((tmp/'ssh_calls').read_text().splitlines())
            except FileNotFoundError:return 0
        (tmp/'ssh_delay').write_text('1.5')
        before=ssh_calls();began=time.monotonic()
        diff=api('/architects/fixture/repos/remote/diff')
        seconds=time.monotonic()-began;calls=ssh_calls()-before
        paths=[f['path'] for f in diff['files']]
        assert seconds>5 and sum(p.startswith('new dir/untracked ') for p in paths)==10,(seconds,paths)
        # One connection for all untracked files, not one each (would be >= 14).
        assert calls<=6,calls
        sha=remote('git -C /home/worker/repo rev-parse HEAD')
        assert api(f'/architects/fixture/repos/remote/commits/{sha}/diff')['files']
        # A remote terminal whose requester gives up still opens, as a tab; a
        # retry meanwhile is refused instead of opening a second shell.
        (tmp/'ssh_delay').write_text('8')
        def tab_ids():return {t.get('id') for t in api(f'/sessions/{sid}/tabs') if t.get('type')=='terminal'}
        tabs_before=tab_ids()
        try:post(f'/sessions/{sid}/terminals',{'workdir_id':'session-primary'},5)
        except (TimeoutError,socket.timeout,urllib.error.URLError):pass
        else:raise AssertionError('terminal answered within 5 s; latency injection ineffective')
        status,body=post(f'/sessions/{sid}/terminals',{'workdir_id':'session-primary'},60)
        assert status==409 and 'still being opened' in body['error']['message'],(status,body)
        new=wait(lambda:tab_ids()-tabs_before,timeout=60)
        assert len(new)==1,new
        late=next(iter(new))
        assert remote(f'tmux -L hiveryn-aux-{late} has-session -t terminal && echo alive')=='alive'
        # A waiting requester gets its terminal after >5 s.
        began=time.monotonic()
        status,body=post(f'/sessions/{sid}/terminals',{'workdir_id':'session-primary'},90)
        assert status in (200,201) and time.monotonic()-began>5,(status,body)
        waited=body['data']['terminal_id']
        assert remote(f'tmux -L hiveryn-aux-{waited} has-session -t terminal && echo alive')=='alive'
        (tmp/'ssh_delay').write_text('0')
        def owned(socket_name):
            with sqlite3.connect(home/'daemon.db') as db:
                return db.execute('select count(*) from remote_resources where session_id=? and socket=?',(sid,socket_name)).fetchone()[0]
        # Closing a terminal removes its server and its ownership record.
        api_delete=urllib.request.Request(f'http://127.0.0.1:{daemonport}/api/sessions/{sid}/terminals/{waited}',method='DELETE')
        urllib.request.urlopen(api_delete,timeout=30).close()
        assert remote(f'tmux -L hiveryn-aux-{waited} has-session 2>/dev/null || echo gone')=='gone'
        assert owned(f'hiveryn-aux-{waited}')==0
        # A creation that fails remotely, where removal cannot be confirmed
        # either (tmux gone), keeps its ownership record for session cleanup.
        run('docker','exec',container,'mv','/usr/bin/tmux','/usr/bin/tmux-hidden')
        try:
            tabs_before=tab_ids()
            status,body=post(f'/sessions/{sid}/terminals',{'workdir_id':'session-primary'},90)
            message=body['error']['message']
            assert status==500 and 'create remote repository terminal on machine box' in message and 'was not confirmed' in message,(status,body)
            assert tab_ids()==tabs_before
            with sqlite3.connect(home/'daemon.db') as db:
                retained=db.execute("select count(*) from remote_resources where session_id=? and socket like 'hiveryn-aux-%'",(sid,)).fetchone()[0]
            assert retained==2,retained  # the late terminal plus the failed one
        finally:run('docker','exec',container,'mv','/usr/bin/tmux-hidden','/usr/bin/tmux')
        api(f'/sessions/{sid}/discard',{})
        assert remote(f'tmux -L hiveryn-aux-{late} has-session 2>/dev/null || echo gone')=='gone'
        print('remote diffs/terminals: >5 s diffs with one SSH call for untracked files, terminal survives requester timeout, retry 409, waiting requester succeeds, close drops ownership, unconfirmed cleanup retained then discarded PASS',flush=True)
        assert 'DATA RACE' not in (tmp/'daemon.log').read_text()
        print('REMOTE FIXTURE PASS (race-instrumented daemon)',flush=True)
    except Exception:
        if (home/'logs/daemon.jsonl').exists(): print((home/'logs/daemon.jsonl').read_text()[-12000:])
        if (tmp/'daemon.log').exists():print((tmp/'daemon.log').read_text()[-12000:])
        if container:print(run('docker','logs',container))
        raise
    finally:
        if daemon:daemon.terminate();daemon.wait(timeout=15)
        if container:run('docker','rm','-f',container)
