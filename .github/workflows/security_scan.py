"""Discover/download the deployed image and gate completed Trivy reports.

Only discover/download receive FLY_READONLY_TOKEN. Rendering is credential-free.
"""
import datetime
import http.client
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

REGISTRY = 'registry.fly.io'
REPOSITORY = 'pyrycode-relay'
IMAGE_PATTERN = r'registry\.fly\.io/pyrycode-relay@sha256:[0-9a-f]{64}'
MAX_RESPONSE = 1024 * 1024


def token():
    value = os.environ.get('FLY_READONLY_TOKEN', '')
    if not value:
        raise ValueError('missing read-only credential')
    return value


def machine_reference(machines):
    if not isinstance(machines, list):
        raise ValueError('invalid Machines response')
    images = set()
    for machine in machines:
        if not isinstance(machine, dict) or not isinstance(machine.get('state'), str):
            raise ValueError('invalid Machine state')
        if machine['state'] != 'started':
            continue
        ref = machine.get('image_ref')
        if not isinstance(ref, dict) or ref.get('registry') != REGISTRY or ref.get('repository') != REPOSITORY:
            raise ValueError('invalid started Machine image reference')
        digest = ref.get('digest')
        if not isinstance(digest, str) or not re.fullmatch(r'sha256:[0-9a-fA-F]{64}', digest):
            raise ValueError('invalid started Machine digest')
        images.add(f'{REGISTRY}/{REPOSITORY}@{digest.lower()}')
    if len(images) != 1:
        raise ValueError('expected exactly one distinct started image')
    return images.pop()


def discover(output):
    credential = token()
    # Direct HTTPSConnection does not follow redirects or forward credentials.
    conn = http.client.HTTPSConnection('api.machines.dev', timeout=30)
    try:
        conn.request('GET', '/v1/apps/pyrycode-relay/machines',
                     headers={'Authorization': 'Bearer ' + credential})
        response = conn.getresponse()
        if response.status != 200:
            raise ValueError('Machines lookup failed')
        data = response.read(MAX_RESPONSE + 1)
        if len(data) > MAX_RESPONSE:
            raise ValueError('Machines response too large')
        image = machine_reference(json.loads(data))
        observed = datetime.datetime.now(datetime.timezone.utc).strftime('%Y-%m-%dT%H:%M:%SZ')
        Path(output).write_text(json.dumps({'image': image, 'observed_at': observed}) + '\n')
        print(f'Deployed image: {image}; observed at {observed} (UTC)')
    finally:
        conn.close()


def metadata(path):
    data = json.loads(Path(path).read_text())
    if not isinstance(data, dict) or not isinstance(data.get('image'), str) or not re.fullmatch(IMAGE_PATTERN, data['image']):
        raise ValueError('invalid discovery metadata')
    observed = data.get('observed_at')
    if not isinstance(observed, str) or not re.fullmatch(r'\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ', observed):
        raise ValueError('invalid observation time')
    datetime.datetime.strptime(observed, '%Y-%m-%dT%H:%M:%SZ')
    return data


def download(discovery, archive):
    credential = token()
    image = metadata(discovery)['image']
    env = {k: v for k, v in os.environ.items() if k != 'FLY_READONLY_TOKEN'}
    # A fresh config contains no credential helpers; Docker persists auth here
    # only, and TemporaryDirectory removes it before the scanner step runs.
    with tempfile.TemporaryDirectory(prefix='relay-registry-') as config:
        env['DOCKER_CONFIG'] = config
        for args, stdin in [
            (['docker', 'login', '--username', 'x', '--password-stdin', REGISTRY], credential + '\n'),
            (['docker', 'pull', image], None),
            (['docker', 'save', '--output', str(archive), image], None),
        ]:
            subprocess.run(args, input=stdin, text=True, env=env, check=True,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=300)


def safe_field(value, allow_empty=False, allow_spaces=False):
    if not isinstance(value, str) or (not value and not allow_empty) or len(value) > 256:
        raise ValueError('invalid finding field')
    if any(ord(c) < (32 if allow_spaces else 33) or ord(c) >= 127 or c in '\"`$\\' for c in value):
        raise ValueError('unsafe finding field')
    return value


def findings(report):
    if not isinstance(report, dict) or report.get('SchemaVersion') != 2 or not isinstance(report.get('Results'), list):
        raise ValueError('unusable Trivy report')
    rows = set()
    for result in report['Results']:
        if not isinstance(result, dict) or not isinstance(result.get('Target'), str) or not result['Target']:
            raise ValueError('invalid Trivy result')
        vulnerabilities = result.get('Vulnerabilities')
        if vulnerabilities is None:
            continue  # Trivy omits this field or emits null for a clean target.
        if not isinstance(vulnerabilities, list):
            raise ValueError('invalid vulnerabilities')
        for vuln in vulnerabilities:
            if not isinstance(vuln, dict):
                raise ValueError('invalid vulnerability')
            severity, advisory, package, installed = [safe_field(vuln.get(k)) for k in
                ['Severity', 'VulnerabilityID', 'PkgName', 'InstalledVersion']]
            if severity not in ('UNKNOWN', 'LOW', 'MEDIUM', 'HIGH', 'CRITICAL'):
                raise ValueError('invalid severity')
            fixed = safe_field(vuln.get('FixedVersion', ''), allow_empty=True, allow_spaces=True).strip()
            if severity in ('CRITICAL', 'HIGH') and fixed:
                rows.add((severity, advisory, package, installed, fixed))
    return sorted(rows, key=lambda row: (0 if row[0] == 'CRITICAL' else 1, *row[1:]))


def render(report_path, discovery, output, run_url):
    data = metadata(discovery)
    rows = findings(json.loads(Path(report_path).read_text()))
    if not rows:
        return 0
    _, advisory, package, installed, _ = rows[0]
    title = f'security-scan regression: {advisory} in deployed-image package {package}@{installed}'
    if len(title) > 256:
        raise ValueError('finding title too long')
    body = ('## Deployed-image security-scan regression\n\n'
            '**Scanner:** Trivy (deployed image)\n'
            f'**Image:** `{data["image"]}`\n'
            f'**Observed at (UTC):** {data["observed_at"]}\n'
            f'**Workflow run:** {run_url}\n\n'
            'Go-binary findings are conservative module-version matches, '
            'rather than source call-path evidence.\n\n### All findings\n\n```\n')
    body += ''.join(f'{s}\t{c}\t{p}@{v}\tfixed in {f}\n' for s, c, p, v, f in rows[:100])
    if len(rows) > 100:
        body += f'(+{len(rows) - 100} more findings)\n'
    body += ('```\n\n### Triage\n'
             '- Patch the dependency or base image, then have the operator deploy.\n'
             '- Check the next deployed-image result and its digest/observation time.\n')
    # The upload guard uses title existence: write it last, after validation.
    Path(output, 'issue-body.md').write_text(body)
    title_marker = Path(output, 'issue-title.txt.tmp')
    title_marker.write_text(title + '\n')
    title_marker.replace(Path(output, 'issue-title.txt'))
    return 1


def main():
    try:
        operation, *args = sys.argv[1:]
        if operation == 'discover' and len(args) == 1:
            discover(*args)
        elif operation == 'download' and len(args) == 2:
            download(*args)
        elif operation == 'render' and len(args) == 4:
            return render(*args)
        else:
            raise ValueError('invalid arguments')
        return 0
    except (ValueError, OSError, http.client.HTTPException, subprocess.SubprocessError):
        # No remote body, exception text or command diagnostics: they can
        # contain reflected credentials, which GitHub masking may miss.
        print('deployed-image scanner failed; no parseable findings artifact', file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
