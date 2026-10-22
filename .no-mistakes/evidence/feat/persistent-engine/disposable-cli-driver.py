import concurrent.futures
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import subprocess
import threading
import time
import urllib.request
import urllib.error

root = Path.cwd() / '.phase-test'
evidence = Path('<test-evidence>')
data = root / 'disposable'
data.mkdir(mode=0o700)
repo = data / 'repository'
repo.mkdir()
binary = str(root / 'tandem')
sessions = {}
counts = {'create': 0, 'prompt': 0, 'interrupt': 0}
lock = threading.RLock()
stop = threading.Event()
drop_event = threading.Event()
event_connections = 0
report = {'runtime': 'controlled HTTP fixture, not native OpenCode or model execution', 'checks': []}

class Native(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass
    def reply(self, value, status=200):
        body = json.dumps(value).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        global event_connections
        path = self.path.split('?')[0]
        if path == '/api/info':
            return self.reply({'version': '2.1.0'})
        if path == '/api/event':
            with lock:
                event_connections += 1
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            try:
                while not stop.wait(.2):
                    if drop_event.is_set():
                        drop_event.clear()
                        return
                    self.wfile.write(b': fixture heartbeat\n\n')
                    self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        with lock:
            if path == '/api/session/active':
                return self.reply({'data': {k: {'type':'running'} for k,s in sessions.items() if s['running']}})
            parts = path.removeprefix('/api/session/').split('/')
            s = sessions.get(parts[0])
            if not s:
                return self.reply({}, 404)
            if len(parts) == 1:
                return self.reply({'data': s['record']})
            if parts[1] in ['permission', 'form', 'inbox']:
                return self.reply({'data': []})
            if parts[1] == 'message' and parts[2] in s['messages']:
                return self.reply({'data': s['messages'][parts[2]]})
            return self.reply({}, 404)
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        v = json.loads(body) if body else {}
        path = self.path.split('?')[0]
        with lock:
            if path == '/api/session':
                counts['create'] += 1
                sessions[v['id']] = {'record': v, 'running': False, 'messages': {}}
                return self.reply({'data': v})
            parts = path.removeprefix('/api/session/').split('/')
            s = sessions[parts[0]]
            if parts[1] == 'prompt':
                counts['prompt'] += 1
                s['running'] = True
                s['messages'][v['id']] = {'id': v['id'], 'type': 'user', 'text': v['text'], 'metadata': v['metadata']}
                return self.reply({'data': {'id': v['id'], 'sessionID': parts[0], 'type': 'user', 'payload': {'text': v['text'], 'metadata': v['metadata']}}})
            if parts[1] == 'interrupt':
                counts['interrupt'] += 1
                active = s['running']
                s['running'] = False
                return self.reply({'interrupted': active})
            return self.reply({}, 404)

native = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Native)
native_thread = threading.Thread(target=native.serve_forever)
native_thread.start()
env = {k:v for k,v in os.environ.items() if not k.startswith(('TANDEM_', 'OPENCODE_'))}
env.update({'TANDEM_TOKEN':'disposable-validation-token-0123456789', 'OPENCODE_URL': f'http://127.0.0.1:{native.server_port}', 'OPENCODE_PASSWORD': 'fixture-only'})
daemon = None
daemon_logs = []

def launch():
    global daemon
    log = (data / f'daemon-{len(daemon_logs)}.log').open('w')
    daemon_logs.append(log)
    daemon = subprocess.Popen([binary, 'serve', '--state-dir', str(data/'state'), '--bind', '127.0.0.1:0', '--allocator', 'git'], env=env, stdout=subprocess.PIPE, stderr=log, text=True)
    receipt = json.loads(daemon.stdout.readline())
    env['TANDEM_URL'] = receipt['url']
    return receipt

def shutdown():
    global daemon
    if daemon is not None:
        daemon.terminate()
        code = daemon.wait(timeout=15)
        assert code == 0, ('daemon exit', code)
        daemon.stdout.close()
        daemon = None

def cli(*args, expected=0):
    r = subprocess.run([binary, *args], env=env, capture_output=True, text=True, timeout=120)
    assert r.returncode == expected, (args, r.returncode, r.stderr)
    return r.stdout

def api(path, value=None, op=None):
    headers = {'Authorization': 'Bearer '+env['TANDEM_TOKEN']}
    if op:
        headers['Idempotency-Key'] = op
    body = json.dumps(value, ensure_ascii=False).encode() if value is not None else None
    headers['Content-Type'] = 'application/json'
    with urllib.request.urlopen(urllib.request.Request(env['TANDEM_URL']+path, data=body, headers=headers), timeout=35) as r:
        return json.load(r)

def detail(task):
    for retry in range(20):
        r = subprocess.run([binary, 'tasks', 'show', task, '--json'], env=env, capture_output=True, text=True, timeout=35)
        if r.returncode == 0:
            return json.loads(r.stdout)
        if 'stale_read' not in r.stderr:
            raise AssertionError(r.stderr)
        # Discard partial output and start a wholly new revision-pinned read.
        time.sleep(.1)
    raise AssertionError('detail never acquired a stable revision')

def wait(predicate):
    deadline = time.monotonic()+40
    while time.monotonic()<deadline:
        try:
            result = predicate()
        except urllib.error.HTTPError as error:
            if error.code != 503:
                raise
            result = False
        if result:
            return result
        time.sleep(.2)
    raise AssertionError('product failed to converge')

def record(name, **values):
    report['checks'].append({'name': name, **values})

try:
    for args in [['init','-b','master'], ['config','core.hooksPath','/dev/null'], ['-c','user.name=Fixture','-c','user.email=fixture@example.invalid','-c','commit.gpgsign=false','commit','--allow-empty','-m','fixture']]:
        subprocess.run(['git','-C',str(repo),*args], check=True, capture_output=True)
    receipt = launch()
    wait(lambda: api('/v1/ready')['ready'])
    project = json.loads(cli('projects','register','--name','Disposable verification','--path',str(repo),'--base-ref','master','--operation-id','project','--json'))
    args = ('tasks','create','--project',project['id'],'--title','Same task across clients','--instructions','Retain worker identity.','--operation-id','duplicate','--json')
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
        futures = [pool.submit(cli, *args) for _ in range(2)]
        tasks = [json.loads(f.result()) for f in futures]
    task = tasks[0]['id']
    assert tasks[1]['id'] == task
    wait(lambda: detail(task)['task']['state']=='running')
    before = detail(task)
    attempt = before['attempts'][0]
    artifact = Path(attempt['workspace']['path'])/'retained.txt'
    artifact.write_text('Client exits and daemon restart must retain this file.')
    record('Concurrent CLI processes return one task and execution', taskId=task, attemptId=attempt['id'], sessionRef=attempt['sessionRef'], runtimeCounts=dict(counts), cli=cli('tasks','list'))
    assert counts == {'create':1, 'prompt':1, 'interrupt':0}
    shutdown()
    restarted = launch()
    assert restarted['instanceId'] == receipt['instanceId']
    wait(lambda: api('/v1/ready')['ready'])
    after = detail(task)
    assert after['attempts'][0]['workspace'] == attempt['workspace']
    assert after['attempts'][0]['sessionRef'] == attempt['sessionRef']
    assert artifact.read_text() == 'Client exits and daemon restart must retain this file.'
    cli('tasks','followup',task,'--text','Continue in the existing session.','--operation-id','continue','--json')
    wait(lambda: counts['prompt']==2)
    assert counts['create']==1
    record('Restart and second client continue the retained worker', instanceId=restarted['instanceId'], taskId=task, attemptId=after['task']['attemptId'], sessionRef=after['attempts'][0]['sessionRef'], runtimeCounts=dict(counts), retainedFile=artifact.read_text())
    other = subprocess.run([binary,'serve','--state-dir',str(data/'state'),'--bind','127.0.0.1:0','--allocator','git'],env=env,capture_output=True,text=True,timeout=10)
    assert other.returncode != 0
    record('Second production daemon is refused', exitCode=other.returncode, stderr=other.stderr.replace(str(data),'<disposable>'))
    second = json.loads(cli('tasks','create','--project',project['id'],'--title','Independent modifying worker','--instructions','Use isolated checkout.','--operation-id','second','--json'))
    wait(lambda: detail(second['id'])['task']['state']=='running')
    second_attempt = detail(second['id'])['attempts'][0]
    assert second_attempt['workspace']['path'] != attempt['workspace']['path']
    assert second_attempt['workspace']['owner'] != attempt['workspace']['owner']
    record('Two modifying workers receive distinct owned checkouts', owners=[attempt['workspace']['owner'], second_attempt['workspace']['owner']], branches=[attempt['workspace']['branch'],second_attempt['workspace']['branch']], runtimeCounts=dict(counts))
    wait(lambda: event_connections >= 2)
    previous_connections = event_connections
    with lock:
        sessions[second_attempt['sessionRef']]['running'] = False
        sessions[second_attempt['sessionRef']]['record']['outcome'] = 'failed'
    drop_event.set()
    wait(lambda: detail(second['id'])['task']['state'] == 'failed')
    wait(lambda: event_connections > previous_connections)
    assert counts['create'] == 2
    record('Dropped native event stream reconnects and converges without duplicate execution', eventConnectionsBefore=previous_connections, eventConnectionsAfter=event_connections, observedTask=detail(second['id'])['task'], runtimeCounts=dict(counts))
    cli('tasks','followup',second['id'],'--text','Recover this existing worker.','--operation-id','recover-second','--json')
    wait(lambda: detail(second['id'])['task']['state'] == 'running')
    text = '界'*43690+'ab'
    assert len(text.encode()) == 128*1024
    ids = set()
    for i in range(65):
        r = subprocess.run([binary,'tasks','create','--project',project['id'],'--title',f'Large task {i}','--instructions','-','--operation-id',f'large-{i}','--json'], input=text,env=env,text=True,capture_output=True,timeout=35)
        assert r.returncode==0, r.stderr
        v=json.loads(r.stdout)
        ids.add(v['id'])
    inventory=json.loads(cli('tasks','list','--json'))
    returned={t['id']:t for t in inventory if t['id'] in ids}
    assert len(returned)==65 and all(t['instructions']==text for t in returned.values())
    manifest=json.loads(cli('snapshot','--json'))
    assert manifest['collections']['tasks']==67
    record('CLI completely traverses 65 valid Unicode 128-KiB tasks', returnedRecords=len(returned), originalBytesPerTask=len(text.encode()), sha256PerTask=hashlib.sha256(text.encode()).hexdigest(), manifest=manifest)
    with lock:
        sessions[attempt['sessionRef']]['running']=False
    time.sleep(2)
    current=detail(task)['task']
    assert current['state']=='running'
    record('Native idleness is not task completion', task=current)
    cli('tasks','result',task,'--attempt',attempt['id'],'--generation',str(current['generation']),'--summary','Verified fixture artifact','--evidence','[{"name":"independent fixture assertion","reference":"fixture://retained-file","passed":true}]','--operation-id','result','--json')
    reviewed=detail(task)
    assert reviewed['task']['state']=='ready_for_review'
    accepted=json.loads(cli('tasks','accept',task,'--operation-id','accept','--json'))
    assert accepted['state']=='completed'
    record('Explicit evidence and review acceptance complete the same task', result=reviewed['result'], acceptedTask=accepted)
    record('Final readable CLI inventory', output=cli('tasks','list'))
    shutdown()
    report['result']='pass'
finally:
    if daemon is not None:
        shutdown()
    stop.set()
    native.shutdown()
    native.server_close()
    native_thread.join(timeout=5)
    for log in daemon_logs:
        log.close()
    (evidence/'production-cli-transcript.json').write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
    shutil.rmtree(data)
print(json.dumps({'result':report.get('result','failed'), 'checks':len(report['checks']), 'evidence':str(evidence/'production-cli-transcript.json')}))
