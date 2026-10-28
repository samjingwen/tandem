import json
import os
from pathlib import Path
import subprocess
import tempfile

root = Path.cwd()
evidence = Path('<test-evidence>')
binary = root / '.tmp/local-test-phase/target/debug/examples/workspace-compat'
records = []
def retain():
    (evidence / 'product-protocol.json').write_text(json.dumps(records, indent=2, ensure_ascii=True).replace(str(base), '<disposable-root>'))
with tempfile.TemporaryDirectory(dir=root / '.tmp/local-test-phase/tmp') as temporary:
    base = Path(temporary)
    env = {key: os.environ[key] for key in ['PATH', 'USER', 'LOGNAME'] if key in os.environ}
    env.update(HOME=str(base / 'home'), XDG_CONFIG_HOME=str(base / 'xdg'), LC_ALL='C', GIT_CONFIG_NOSYSTEM='1')
    env['PATH'] = str(root / '.tmp/worktrunk/bin') + os.pathsep + env['PATH']
    for name in ['home', 'xdg', 'repo']:
        (base / name).mkdir()
    repo = base / 'repo'
    def git(path, *args):
        return subprocess.check_output(['git', '-C', str(path), *args], env=env, stderr=subprocess.STDOUT, text=True).strip()
    git(repo, 'init', '-b', 'master')
    git(repo, '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid', 'commit', '--allow-empty', '-m', 'fixture')
    project = {'id': 'prj_probe', 'name': 'Probe', 'path': str(repo), 'baseRef': 'master'}
    def call(method, tree, use_wt=False, workspace=None, **extra):
        request = {'root': str(tree), 'useWorktrunk': use_wt, 'method': method, 'project': project, **extra}
        if workspace is not None:
            request['workspace'] = workspace
        result = subprocess.run([str(binary)], input=json.dumps(request), capture_output=True, text=True, env=env, timeout=15, check=True)
        response = json.loads(result.stdout)
        records.append({'request': request, 'response': response})
        retain()
        return response
    for use_wt in [False, True]:
        tree = base / ('wt-trees' if use_wt else 'git-trees')
        planned = call('plan', tree, use_wt, owner='run_wt' if use_wt else 'run_git')['value']
        assert not tree.exists()
        allocated = call('ensure', tree, use_wt, planned)['value']
        path = Path(allocated['path'])
        git(path, '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid', 'commit', '--allow-empty', '-m', 'worker')
        head = git(path, 'rev-parse', 'HEAD')
        recovered = call('ensure', tree, use_wt, planned)['value']
        assert recovered['baseSha'] == head
        (path / 'keep').write_text('worker data')
        assert 'error' in call('remove', tree, use_wt, allocated)
        assert (path / 'keep').read_text() == 'worker data'
        (path / 'keep').unlink()
        assert 'error' not in call('remove', tree, use_wt, allocated)
        assert not path.exists()
        assert git(repo, 'rev-parse', allocated['branch']) == head
        records.append({'asserted': 'pure plan; current HEAD recovery; dirty refusal preserves bytes; clean removal retains worker branch', 'allocator': use_wt, 'retainedHead': head})
    for index, name in enumerate(['bell-\x07', 'vertical-\x0b', 'escape-\x1b', 'literal-^G', 'literal-^K', 'literal-^[', 'unicode-é-backslash-\\xc3']):
        tree = base / name
        planned = call('plan', tree, owner=f'run_holder{index}')['value']
        allocated = call('ensure', tree, workspace=planned)['value']
        path = Path(allocated['path'])
        holder = subprocess.Popen(['sh', '-c', 'read release'], cwd=path, stdin=subprocess.PIPE, env=env)
        try:
            inventory = subprocess.run(['lsof', '-nP', '-a', '-d', 'cwd', '-Fpn', '-p', str(holder.pid)], env=env, text=True, capture_output=True, check=True, timeout=5)
            records.append({'holderPathKind': name, 'lsof': inventory.stdout})
            assert 'error' in call('remove', tree, workspace=allocated)
            assert holder.poll() is None and path.is_dir()
        finally:
            holder.communicate(b'release\n', timeout=5)
        assert holder.returncode == 0
        after = call('remove', tree, workspace=allocated)
        if name.startswith('unicode'):
            assert 'error' not in after
        else:
            assert 'error' in after and 'ambiguous' in after['error']['message']
            assert path.is_dir()
        records.append({'asserted': 'live holder preserved without signal; fixture holder released and joined; ambiguous inventory remains fail-closed', 'pathKind': name})
text = json.dumps(records, indent=2, ensure_ascii=True).replace(str(base), '<disposable-root>')
(evidence / 'product-protocol.json').write_text(text)
print('Product protocol assertions passed; all fixture holders released and joined.')
