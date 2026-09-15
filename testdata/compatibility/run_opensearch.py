#!/usr/bin/env python3
"""Replay compatibility fixtures against a disposable OpenSearch server."""
import argparse
import json
from pathlib import Path
import urllib.error
import urllib.request
import uuid


def request(url, method, path, body=''):
    headers = {'Content-Type': 'application/x-ndjson' if path == '/_bulk' else 'application/json'}
    req = urllib.request.Request(url.rstrip('/') + path, data=body.encode() if body else None,
                                 headers=headers, method=method)
    try:
        response = urllib.request.urlopen(req, timeout=60)
    except urllib.error.HTTPError as error:
        response = error
    with response:
        return response.status, json.load(response)


def pointer(value, path):
    for part in path.lstrip('/').split('/'):
        part = part.replace('~1', '/').replace('~0', '~')
        value = value[int(part)] if isinstance(value, list) else value[part]
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--url', required=True, help='URL of a disposable test server')
    parser.add_argument('--report', required=True)
    args = parser.parse_args()
    code, server = request(args.url, 'GET', '/')
    if code != 200 or server.get('version', {}).get('number') != '3.8.0':
        raise SystemExit('Expected OpenSearch 3.8.0: ' + json.dumps(server))
    cases = json.loads(Path(__file__).with_name('probes.json').read_text())
    observations = []
    for case in cases:
        # Isolate templates and indices, including mapping response keys.
        prefix = 'osmem-probe-' + uuid.uuid4().hex
        tc = json.loads(json.dumps(case).replace('audit-', prefix + '-'))
        observation = {'id': case['id'], 'source': case['source'], 'differences': []}
        try:
            for step in tc['setup']:
                code, body = request(args.url, step['method'], step['path'], step['body'])
                if code != step['status']:
                    raise RuntimeError(f'Setup {step["path"]}: {code} {body}')
            step = tc['request']
            code, body = request(args.url, step['method'], step['path'], step['body'])
            observation.update(status=code, body=body)
            if code != step['status']:
                observation['differences'].append(f'status: want {step["status"]}, got {code}')
            for path, want in step.get('checks', {}).items():
                try:
                    got = pointer(body, path)
                except (KeyError, IndexError, TypeError, ValueError):
                    observation['differences'].append(f'{path}: missing, want {want!r}')
                    continue
                if got != want:
                    observation['differences'].append(f'{path}: want {want!r}, got {got!r}')
            for step in tc.get('after', []):
                code, body = request(args.url, step['method'], step['path'], step['body'])
                observation.setdefault('after', []).append({'path': step['path'], 'status': code, 'body': body})
                if code != step['status']:
                    observation['differences'].append(f'after {step["path"]}: want {step["status"]}, got {code}')
        except Exception as error:
            observation['setup_or_transport_error'] = str(error)
        finally:
            # Only delete exact resource names owned by this scenario.
            for path in [f'/{prefix}-i', f'/_template/{prefix}-legacy',
                         f'/_index_template/{prefix}-modern', f'/_index_template/{prefix}-t1',
                         f'/_index_template/{prefix}-t2']:
                try:
                    status, body = request(args.url, 'DELETE', path)
                    if status not in (200, 404):
                        raise RuntimeError(f'{status}: {body}')
                except Exception as error:
                    observation.setdefault('cleanup_errors', []).append(f'{path}: {error}')
        observations.append(observation)
        print(case['id'], observation['differences'], observation.get('setup_or_transport_error', ''))
    Path(args.report).write_text(json.dumps({'server': server, 'observations': observations}, indent=2) + '\n')
    return int(any(o['differences'] or o.get('setup_or_transport_error') or o.get('cleanup_errors') for o in observations))


if __name__ == '__main__':
    raise SystemExit(main())
