import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

root = Path.cwd()
evidence = Path('<test-evidence>')
evidence.mkdir(parents=True, exist_ok=True)
scratch = root / '.tmp/local-test-phase'
scratch.mkdir()
(scratch / 'tmp').mkdir()
env = os.environ.copy()
env.update(TMPDIR=str(scratch / 'tmp'), CARGO_TARGET_DIR=str(scratch / 'target'), PYTHONDONTWRITEBYTECODE='1')
env['PATH'] = str(root / '.tmp/worktrunk/bin') + os.pathsep + env['PATH']
for name, binary in [('WORKSPACE', 'workspace-compat'), ('STORE', 'store-compat'), ('ENGINE', 'engine-compat'), ('OPENCODE', 'runtime-compat')]:
    env['TANDEM_RUST_' + name + '_HELPER'] = str(scratch / 'target/debug/examples' / binary)
steps = [
    ('prepare', ['make', 'worktrunk-prepare']),
    ('workspace-rust', ['cargo', 'test', '--locked', '-p', 'tandem-workspace', '--all-targets', '--', '--nocapture']),
    ('consumers-build', ['cargo', 'build', '--locked', '--example', 'workspace-compat', '--example', 'store-compat', '--example', 'engine-compat', '--example', 'runtime-compat', '--example', 'engine-safety']),
    ('workspace-oracle', ['go', 'test', '-v', '-race', './internal/workspace', '-run', '^TestRustWorkspace', '-count=1']),
    ('store-engine-oracle', ['go', 'test', '-v', './internal/store', '-run', '^TestRust(Store|Engine)', '-count=1']),
    ('runtime-oracle', ['go', 'test', '-v', '-race', './internal/opencode', '-run', '^TestRustOpenCode', '-count=1']),
    ('engine-safety', ['python3', 'tools/test-engine-safety.py', str(scratch / 'target/debug/examples/engine-safety')]),
    ('installer', ['make', 'python-test']),
    ('integration', ['go', 'test', '-v', './internal/integration', '-run', '^Test(IndependentClientsDuplicateCreateDisconnectAndRestart|AnotherClientAnswersDecisionAndAcceptsCurrentGeneration|TaskCleanupResolvesOwnedAttempt)$', '-count=1']),
]
results = []
for name, command in steps:
    with (evidence / (name + '.log')).open('w') as log:
        log.write('$ ' + ' '.join(command) + '\n')
        log.flush()
        result = subprocess.run(command, env=env, stdout=log, stderr=subprocess.STDOUT)
        log.write('\nEXIT STATUS: ' + str(result.returncode) + '\n')
    results.append({'step': name, 'command': command, 'exit_status': result.returncode})
    print(name, result.returncode, flush=True)
    (evidence / 'results.json').write_text(json.dumps(results, indent=2))
hashes = {}
for path in (scratch / 'target/debug/examples').glob('*'):
    if path.is_file() and path.suffix == '':
        hashes[path.name] = hashlib.sha256(path.read_bytes()).hexdigest()
hashes['wt'] = hashlib.sha256((root / '.tmp/worktrunk/bin/wt').read_bytes()).hexdigest()
(evidence / 'build-identity.json').write_text(json.dumps({'commit': subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip(), 'hashes': hashes}, indent=2))
sys.exit(1 if any(r['exit_status'] for r in results) else 0)
