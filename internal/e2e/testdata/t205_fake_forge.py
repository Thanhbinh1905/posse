#!/usr/bin/env python3
"""Stateful offline gh/glab fixture for PR body ownership marker E2Es."""
import json
import os
from pathlib import Path
import subprocess
import sys

root = Path(os.environ['POSSE_TEST_ROOT'])
args = sys.argv[1:]
is_gitlab = Path(sys.argv[0]).name == 'glab'
state_path = root / 't199-forge.json'
log_path = root / 't199-forge-calls.jsonl'
with log_path.open('a') as stream:
    stream.write(json.dumps({'forge': 'gitlab' if is_gitlab else 'github', 'args': args}) + '\n')
state = json.loads(state_path.read_text()) if state_path.exists() else None


def flag(name, default=''):
    return args[args.index(name) + 1] if name in args else default


def head():
    return subprocess.check_output(['git', '--git-dir=' + os.environ['POSSE_TEST_REMOTE'], 'rev-parse', 'refs/heads/posse/pr-lifecycle-change'], text=True).strip()


def metadata():
    sha = head()
    if is_gitlab:
        return dict(state, state='opened', sha=sha, source_branch='posse/pr-lifecycle-change', target_branch='main', source_project_id=7, target_project_id=7)
    return dict(state, state='OPEN', headRefOid=sha, headRefName='posse/pr-lifecycle-change', baseRefName='main', headRepository={'nameWithOwner': 'acme/shop'})


def emit(value):
    print(json.dumps(value))


def save():
    state_path.write_text(json.dumps(state))


def fail(stage):
    if os.environ.get('T199_FAIL') == stage:
        print('Injected fake forge ' + stage + ' failure', file=sys.stderr)
        sys.exit(1)


if args[:2] == ['auth', 'status']:
    print('authenticated')
elif args[:2] == ['api', 'graphql']:
    print((root / 'gh-state.json').read_text())
elif (not is_gitlab and args[:2] == ['pr', 'list']) or (is_gitlab and any('merge_requests?state=opened' in a for a in args)):
    if not state:
        emit([])
    elif is_gitlab:
        emit([metadata()])
    else:
        emit([dict(url=state['url'], headRefName='posse/pr-lifecycle-change', headRefOid=head())])
elif args[:2] in (['pr', 'create'], ['mr', 'create']):
    state = {'title': flag('--title')}
    if is_gitlab:
        state.update(web_url='https://git.example.com/group/sub/shop/-/merge_requests/17', description=flag('--description'))
        print(state['web_url'])
    else:
        state.update(url='https://github.com/acme/shop/pull/17', body=flag('--body'))
        print(state['url'])
    save()
elif args[:2] == ['pr', 'edit'] or (is_gitlab and flag('--method') == 'PUT'):
    fail('write')
    if is_gitlab:
        for argument in args:
            if argument.startswith('title='):
                state['title'] = argument[len('title='):]
            elif argument.startswith('description='):
                state['description'] = argument[len('description='):]
    else:
        state.update(title=flag('--title'), body=flag('--body'))
    save()
    fail('write-applied')
    print('updated')
elif args[:2] == ['pr', 'view']:
    if flag('--json') == 'title,body':
        fail('read')
        emit({key: state.get(key, '') for key in flag('--json').split(',')})
    else:
        emit(metadata())
elif is_gitlab and args[0] == 'api' and any(a.endswith('/merge_requests/17') for a in args):
    emit(metadata())
else:
    print('Unexpected fake forge command: ' + repr(args), file=sys.stderr)
    sys.exit(90)
