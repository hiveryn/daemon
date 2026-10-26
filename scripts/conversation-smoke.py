#!/usr/bin/env python3
"""Isolated HTTP contract smoke with a deterministic structured Codex fixture.
Never connects to an existing daemon, reads authentication, or launches a model.
Use --keep to retain the temporary state and evidence for inspection.
"""
import argparse
import concurrent.futures
import http.server
import json
import os
import pathlib
import signal
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[1]

FIXTURE = r'''import json,sys,time,uuid
thread='fixture-thread'
def emit(v): print(json.dumps(v),flush=True)
for line in sys.stdin:
 r=json.loads(line);method=r['method'];p=r.get('params',{});rid=r['id']
 if method=='initialize':emit({'id':rid,'result':{}})
 elif method in ['thread/start','thread/resume']:emit({'id':rid,'result':{'thread':{'id':thread}}})
 elif method=='thread/read':emit({'id':rid,'result':{'thread':{'id':thread,'turns':[]}}})
 elif method=='turn/start':
  tid=str(uuid.uuid4());emit({'id':rid,'result':{'turn':{'id':tid}}})
  text=p['input'][0]['text'];time.sleep(.08)
  for item in [{'id':'u-'+tid,'type':'userMessage','content':[{'type':'text','text':text}]},{'id':'a-'+tid,'type':'agentMessage','text':'ACK'}]:
   emit({'method':'item/completed','params':{'threadId':thread,'turnId':tid,'item':item}})
  emit({'method':'turn/completed','params':{'threadId':thread,'turn':{'id':tid,'status':'completed'}}})
 else:emit({'id':rid,'error':{'message':'unsupported fixture method '+method}})
'''


def port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1', 0))
        return sock.getsockname()[1]


def wait(fn, timeout=30):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            result = fn()
            if result:
                return result
        except (OSError, urllib.error.URLError) as error:
            last = error
        time.sleep(.1)
    raise AssertionError(f'timed out: {last}')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--keep', action='store_true')
    args = parser.parse_args()
    (ROOT / 'runs').mkdir(exist_ok=True)
    scratch = tempfile.TemporaryDirectory(prefix='conversation-smoke-', dir=ROOT / 'runs')
    root = pathlib.Path(scratch.name)
    process = None
    notifier = None
    evidence = {}
    daemon_port, notify_port = port(), port()
    base = f'http://127.0.0.1:{daemon_port}'

    def call(path, body=None, expected=200):
        request = urllib.request.Request(base + path, data=json.dumps(body).encode() if body is not None else None,
                                         headers={'Content-Type': 'application/json'})
        try:
            response = urllib.request.urlopen(request, timeout=30)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            payload = json.load(response)
            assert response.status == expected, (response.status, payload)
            return payload.get('data') if expected < 400 else payload

    def start():
        nonlocal process
        process = subprocess.Popen([str(root / 'hiverynd')], env=env,
                                   stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        wait(lambda: call('/api/health'))

    def stop():
        if process is not None and process.poll() is None:
            process.send_signal(signal.SIGTERM)
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
                raise AssertionError('owned daemon did not shut down within 15 seconds')

    class NotificationSink(http.server.BaseHTTPRequestHandler):
        def do_POST(self):
            self.rfile.read(int(self.headers['Content-Length']))
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b'{}')

        def log_message(self, *_):
            pass

    try:
        for name in ['home', 'workspace', 'workspace/workflows', 'repo', 'providers']:
            (root / name).mkdir()
        subprocess.run(['git', 'init', '-q', str(root / 'repo')], check=True)
        fixture = root / 'providers/codex'
        fixture.write_text(f'#!{os.sys.executable}\n' + FIXTURE)
        fixture.chmod(0o700)
        (root / 'workspace/hiveryn.yaml').write_text(f'name: HTTP fixture\nrepos:\n  fixture: {root / "repo"}\n')
        (root / 'workspace/OVERVIEW.md').write_text('---\nupdated_at: 2026-10-26T09:00:00Z\n---\nSynthetic fixture.\n')
        (root / 'home/architects.yaml').write_text(f'fixture: {root / "workspace"}\n')
        (root / 'home/variants.yaml').write_text('fixture:\n  agent: codex\n')
        (root / 'home/config.yaml').write_text(f'port: {daemon_port}\nbind_address: 127.0.0.1\nlog_level: info\nnotifications:\n  ntfy:\n    server: http://127.0.0.1:{notify_port}\n    topic: fixture\n')
        notifier = http.server.ThreadingHTTPServer(('127.0.0.1', notify_port), NotificationSink)
        threading.Thread(target=notifier.serve_forever, daemon=True).start()
        subprocess.run(['go', 'build', '-o', str(root / 'hiverynd'), './cmd/hiverynd'], cwd=ROOT, check=True)
        env = {key: value for key, value in os.environ.items() if not key.startswith(('HIVERYN', 'AGENTRUNTIME', 'CODEX', 'CLAUDE', 'OPENCODE', 'ANTHROPIC', 'OPENAI'))}
        env.update(HIVERYN_HOME=str(root / 'home'), PATH=str(root / 'providers') + os.pathsep + env['PATH'])
        with (root / 'daemon.log').open('w') as log:
            start()
            ticket = call('/api/architects/fixture/tickets', {'title': 'HTTP conversation fixture', 'repo': 'fixture', 'body': 'Synthetic fixture.'}, 201)
            session = call('/api/sessions', {'session_type': 'ticket', 'architect_key': 'fixture', 'ticket_id': ticket['id'], 'interaction_mode': 'chat'}, 201)
            sid = session['id']
            prefix = '/api/sessions/' + sid
            launch = call(prefix + '/runs', {'profile_name': 'fixture'}, 201)
            assert 'main_terminal_id' not in launch and 'ws_url' not in launch
            assert any(s['id'] == sid and s['conversation']['send'] for s in call('/api/sessions')['sessions'])
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
                receipts = list(pool.map(lambda i: call(prefix + '/messages', {'request_id': f'input-{i}', 'text': f'hello {i}'}, 202), range(8)))
            replay = call(prefix + '/messages', {'request_id': 'input-0', 'text': 'hello 0'}, 202)
            assert replay['id'] == receipts[0]['id']
            call(prefix + '/messages', {'request_id': 'input-0', 'text': 'different'}, 409)
            for receipt in receipts:
                wait(lambda: call(prefix + '/submissions/' + receipt['id'])['status'] == 'completed')
            submissions = []
            sp = call(prefix + '/submissions?limit=2')
            while True:
                submissions.extend(sp['submissions'])
                if not sp['has_more']:
                    break
                sp = call(prefix + f'/submissions?limit=2&after_sequence={sp["next_sequence"]}')
            assert len(submissions) == 9 and len({s['id'] for s in submissions}) == 9
            assert all(s['text'] == '' for s in submissions if s['origin'] == 'kickoff')
            call(prefix + '/submissions?after_sequence=-1', expected=400)
            page = call(prefix + '/conversation?limit=2')
            snapshot = page['snapshot_revision']
            items = page['items']
            while page['has_more']:
                page = call(prefix + f'/conversation?limit=2&after_revision={page["next_revision"]}&snapshot_revision={snapshot}')
                items.extend(page['items'])
            assert any(item['role'] == 'assistant' and item.get('text') == 'ACK' for item in items)
            assert all('Synthetic fixture.' not in item.get('text', '') for item in items)
            with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
                question = pool.submit(call, prefix + '/questions', {'question': 'Fixture choice?', 'answers': ['A', 'B'], 'recommended_index': 0})
                required = wait(lambda: next((e for e in call(prefix + '/controls')['events'] if e['type'] == 'question' and e['status'] == 'required'), None))
                call(prefix + '/questions/' + required['raw']['question_id'] + '/answer', {'answer': 'B'})
                evidence['question_answer'] = question.result()
            stop()
            start()
            receipt = call(prefix + '/messages', {'request_id': 'resumed', 'text': 'resumed hello'}, 202)
            wait(lambda: call(prefix + '/submissions/' + receipt['id'])['status'] == 'completed')
            queued = call(prefix + '/messages', {'request_id': 'discarded', 'text': 'queued'}, 202)
            call(prefix + '/discard', {})
            settled = call(prefix + '/submissions/' + queued['id'])
            assert settled['status'] in ['failed', 'completed']
            replay = call(prefix + '/messages', {'request_id': 'discarded', 'text': 'queued'}, 202)
            assert replay['id'] == queued['id']
            archives = call('/api/archived-chats?architect_key=fixture&limit=1')
            assert len(archives['chats']) == 1 and archives['chats'][0]['id'] == sid
            assert archives['chats'][0]['lifecycle'] == 'discarded'
            assert archives['chats'][0]['agent'] == 'codex'
            assert not any(s['id'] == sid for s in call('/api/sessions')['sessions'])
            call(prefix + '/messages', {'request_id': 'new-after-end', 'text': 'must not run'}, 409)
            call(prefix + '/runs', {'profile_name': 'fixture'}, 404)
            retained = call(prefix + '/conversation')
            assert not any(i['role'] == 'tool' and i['status'] == 'started' for i in {i['id']: i for i in retained['items']}.values())
            assert call(prefix + '/submissions')['submissions']
            stop()
            start()
            assert call('/api/archived-chats')['chats'][0]['lifecycle'] == 'discarded'
            evidence['archived_chat'] = archives['chats'][0]
            evidence.update(result='passed' , receipts=receipts, snapshot_revision=snapshot, discarded=settled)
            stop()
        (root / 'evidence.json').write_text(json.dumps(evidence, indent=2))
        print('Passed isolated create/list/read/send/retry/conflict/poll/answer/restart/discard HTTP smoke.')
    finally:
        stop()
        if notifier:
            notifier.shutdown()
        if args.keep:
            scratch._finalizer.detach()
            print('Retained evidence:', root)
        else:
            scratch.cleanup()


if __name__ == '__main__':
    main()
