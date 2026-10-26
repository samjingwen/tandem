import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
import tempfile

binary = Path('target/debug/examples/store-compat').absolute()
evidence = Path('<test-evidence>')
root = Path(tempfile.mkdtemp(prefix='fixtures-', dir='.tmp/test-phase-consumer')).absolute()
events = []

def run(mode, directory, code=0):
    result = subprocess.run([str(binary), mode, str(directory)], capture_output=True, text=True, timeout=20)
    assert result.returncode == code, (mode, result.returncode, result.stdout, result.stderr)
    return result

def refuse(directory, expected):
    result = run('snapshot', directory, 1)
    error = json.loads(result.stderr)['error']
    assert error['code'] == expected, error
    return error

try:
    state = root / 'state'
    seed = json.loads(run('seed', state).stdout)
    (evidence / 'rust-seed-snapshot.json').write_text(json.dumps(seed, ensure_ascii=False, indent=2))
    db = sqlite3.connect(state / 'tandem.db')
    # Synthetic legacy spelling and future extension are owned persisted data contracts.
    body = '{"id":"tsk_go","title":"before","createdAt":"2026-10-26T03:04:05.123456789+00:00","extension":{"large":9007199254740993,"decimal":1.0000000000000000001,"escaped":"\\u003c","null":null}}'
    db.execute('INSERT INTO tasks VALUES (?,?)', ('tsk_go', body))
    db.commit()
    def durable():
        return {table: db.execute('SELECT * FROM '+table+' ORDER BY 1,2').fetchall() for table in ['metadata','projects','tasks','attempts','commands','decisions','results','operations','history']}
    before = durable()
    run('noop', state)
    assert durable() == before
    changed = json.loads(run('update', state).stdout)
    stored = db.execute('SELECT body FROM tasks WHERE id=?', ('tsk_go',)).fetchone()[0]
    assert changed['state']['tasks']['tsk_go']['title'] == 'written-by-rust 世界'
    assert changed['state']['tasks']['tsk_go']['generation'] == 1
    after = durable()
    for table in ['projects','attempts','commands','decisions','results','operations']:
        assert before[table] == after[table]
    assert dict(after['tasks'])['tsk_rust'] == dict(before['tasks'])['tsk_rust']
    assert '2026-10-26T03:04:05.123456789+00:00' in stored
    # Python's Decimal is an independent precision oracle for changed persisted JSON.
    from decimal import Decimal
    original = json.loads(body, parse_float=Decimal)
    updated = json.loads(stored, parse_float=Decimal)
    assert original['extension'] == updated['extension']
    assert updated['extension']['decimal'] == Decimal('1.0000000000000000001')
    assert len(after['history']) == len(before['history'])+1
    events.append({'scenario':'legacy_extension_timestamp_and_quiet_noop','stored_changed_task_json':stored,'untouched_receipt_body':after['operations'][0][1],'noop_all_rows_byte_identical':True,'history_delta_for_single_update':1})
    db.close()

    # A live lease is held until stdin closes, including access through an alias.
    hold = subprocess.Popen([str(binary),'hold',str(state)],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
    try:
        assert hold.stdout.readline().strip() == '{"locked":true}'
        alias = root/'alias'
        alias.symlink_to(state, target_is_directory=True)
        for path in [state,alias]:
            events.append({'scenario':'live_owner_rejected_alias' if path==alias else 'live_owner_rejected','error':refuse(path,'store_owned')})
        hold.stdin.close()
        assert hold.wait(timeout=20) == 0
    finally:
        if hold.poll() is None:
            hold.kill()
            hold.wait(timeout=20)
    run('snapshot',state)
    events.append({'scenario':'graceful_lease_release','reopen_succeeded':True,'child_exit':hold.returncode})

    db = sqlite3.connect(state/'tandem.db')
    db.execute("INSERT INTO tasks VALUES ('bad','[]')")
    db.commit()
    events.append({'scenario':'malformed_record_refused','error':refuse(state,'corrupt_state')})
    db.execute("DELETE FROM tasks WHERE id='bad'")
    db.execute("INSERT INTO schema_migrations VALUES (2,'synthetic future')")
    db.commit()
    events.append({'scenario':'future_schema_refused','error':refuse(state,'newer_schema')})
    db.close()

    public = root/'public'
    public.mkdir(mode=0o755)
    public.chmod(0o755)
    err = refuse(public,'invalid_request')
    assert public.stat().st_mode & 0o777 == 0o755
    assert not (public/'tandem.db').exists()
    events.append({'scenario':'nonprivate_directory_not_repaired','error':err,'mode_unchanged':'0755','database_created':False})
    for name in ['scheduler.lock','tandem.db','tandem.db-wal','tandem.db-shm']:
        directory = root/('nonregular-'+name)
        directory.mkdir(mode=0o700)
        outside = root/('sentinel-'+name)
        outside.write_text('must remain unchanged')
        (directory/name).symlink_to(outside)
        err = refuse(directory,'invalid_request')
        assert outside.read_text() == 'must remain unchanged'
        events.append({'scenario':'nonregular_file_refused','name':name,'error':err,'target_unchanged':True})
    (evidence/'live-fixture-transcript.json').write_text(json.dumps(events,ensure_ascii=False,indent=2)+'\n')
    print(json.dumps(events,ensure_ascii=False,indent=2))
finally:
    shutil.rmtree(root)
