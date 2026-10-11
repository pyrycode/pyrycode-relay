"""Offline fixtures and command stubs for the deployed-image workflow."""
import copy
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest
from unittest.mock import patch, MagicMock

HERE = Path(__file__).resolve().parent
DIGEST = 'sha256:' + 'a' * 64
REF = 'registry.fly.io/pyrycode-relay@' + DIGEST
OBSERVATION = {'image': REF, 'observed_at': '2026-10-11T06:00:00Z'}
MACHINE = {'state': 'started', 'config': {'image': 'wrong:tag'}, 'image_ref': {
    'registry': 'registry.fly.io', 'repository': 'pyrycode-relay', 'digest': DIGEST}}
VULN = {'VulnerabilityID': 'CVE-2026-1234', 'PkgName': 'stdlib',
        'InstalledVersion': 'v1.26.0', 'FixedVersion': '1.25.8, 1.26.1', 'Severity': 'HIGH'}
REPORT = {'SchemaVersion': 2, 'Results': [{'Target': '/pyrycode-relay',
                                         'Vulnerabilities': [VULN]}]}


def load_helper():
    spec = importlib.util.spec_from_file_location('security_scan', HERE / 'security_scan.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class DeployedScanTests(unittest.TestCase):
    def setUp(self):
        self.scan = load_helper()
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.meta = self.root / 'deployment.json'
        self.meta.write_text(json.dumps(OBSERVATION))
        self.report = self.root / 'trivy.json'

    def test_discovery(self):
        stopped = {'state': 'stopped', 'image_ref': None}
        self.assertEqual(self.scan.machine_reference([MACHINE, stopped, MACHINE]), REF)
        malformed = []
        for field, value in [('registry', 'evil.example'), ('repository', 'other'),
                             ('digest', 'latest'), ('digest', 'sha256:' + 'a' * 63),
                             ('digest', None), ('digest', 'sha256:' + 'g' * 64)]:
            machine = copy.deepcopy(MACHINE)
            machine['image_ref'][field] = value
            malformed.append([MACHINE, machine])
        different = copy.deepcopy(MACHINE)
        different['image_ref']['digest'] = 'sha256:' + 'b' * 64
        for machines in [[], [stopped], {}, [None], [MACHINE, different],
                         [MACHINE, {'state': 'started'}], *malformed]:
            with self.subTest(machines=machines), self.assertRaises(ValueError):
                self.scan.machine_reference(machines)

    def test_lookup_and_secret_hygiene(self):
        conn = MagicMock()
        response = conn.getresponse.return_value
        response.status = 200
        response.read.return_value = json.dumps([MACHINE]).encode()
        with patch.dict(os.environ, {'FLY_READONLY_TOKEN': 'secret-canary'}), \
                patch.object(self.scan.http.client, 'HTTPSConnection', return_value=conn), \
                patch('builtins.print') as log:
            self.scan.discover(self.meta)
        method, endpoint = conn.request.call_args.args
        self.assertEqual((method, endpoint), ('GET', '/v1/apps/pyrycode-relay/machines'))
        self.assertIn('secret-canary', conn.request.call_args.kwargs['headers']['Authorization'])
        result = json.loads(self.meta.read_text())
        self.assertEqual(result['image'], REF)
        self.assertRegex(result['observed_at'], r'^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$')
        logged = str(log.call_args_list)
        self.assertIn(REF, logged)
        self.assertIn(result['observed_at'], logged)
        self.assertNotIn('secret-canary', logged + self.meta.read_text())
        conn.close.assert_called_once()
        for status, data in [(401, b'secret-canary'), (200, b'{'), (200, b'x' * (1048576 + 1))]:
            with self.subTest(status=status, size=len(data)):
                self.meta.unlink(missing_ok=True)
                response.status, response.read.return_value = status, data
                with patch.dict(os.environ, {'FLY_READONLY_TOKEN': 'secret-canary'}), \
                        patch.object(self.scan.http.client, 'HTTPSConnection', return_value=conn), \
                        self.assertRaises(ValueError):
                    self.scan.discover(self.meta)
                self.assertFalse(self.meta.exists())

    def test_missing_credentials(self):
        with patch.dict(os.environ, {}, clear=True):
            for operation in [lambda: self.scan.discover(self.meta),
                              lambda: self.scan.download(self.meta, self.root / 'image.tar')]:
                with self.assertRaises(ValueError):
                    operation()

    def test_download_cleanup(self):
        for failed_step in [None, 'login', 'pull', 'save']:
            configs, commands = [], []

            def docker(args, **kwargs):
                commands.append(args)
                config = Path(kwargs['env']['DOCKER_CONFIG'])
                configs.append(config)
                self.assertEqual(config.stat().st_mode & 0o777, 0o700)
                (config / 'config.json').write_text('persisted-canary')
                self.assertNotIn('FLY_READONLY_TOKEN', kwargs['env'])
                self.assertNotIn('secret-canary', str(args))
                self.assertEqual(kwargs['stdout'], subprocess.DEVNULL)
                self.assertEqual(kwargs['stderr'], subprocess.DEVNULL)
                if args[1] == 'login':
                    self.assertEqual(kwargs['input'], 'secret-canary\n')
                    self.assertIn('--password-stdin', args)
                else:
                    self.assertIsNone(kwargs.get('input'))
                    self.assertIn(REF, args)
                if args[1] == failed_step:
                    raise subprocess.CalledProcessError(1, args)
                if args[1] == 'save':
                    Path(args[args.index('--output') + 1]).write_bytes(b'archive')
                return subprocess.CompletedProcess(args, 0)

            with self.subTest(failed_step=failed_step), \
                    patch.dict(os.environ, {'FLY_READONLY_TOKEN': 'secret-canary'}), \
                    patch.object(self.scan.subprocess, 'run', side_effect=docker):
                if failed_step:
                    with self.assertRaises(subprocess.CalledProcessError):
                        self.scan.download(self.meta, self.root / 'image.tar')
                else:
                    self.scan.download(self.meta, self.root / 'image.tar')
                    self.assertEqual([c[1] for c in commands], ['login', 'pull', 'save'])
            self.assertTrue(configs)
            self.assertTrue(all(not c.exists() for c in configs))

    def render(self, report):
        self.report.write_text(json.dumps(report))
        return self.scan.render(self.report, self.meta, self.root, 'https://github.com/o/r/actions/runs/42')

    def test_cli_invalid_input_and_missing_token(self):
        self.report.write_text(json.dumps(REPORT)[:-1])
        for args in [['render', str(self.report), str(self.meta), str(self.root), 'run'],
                     ['discover', str(self.root / 'missing.json')]]:
            env = {k: v for k, v in os.environ.items() if k != 'FLY_READONLY_TOKEN'}
            result = subprocess.run(['python3', str(HERE / 'security_scan.py'), *args],
                                    env=env, text=True, capture_output=True)
            self.assertEqual(result.returncode, 2)
            self.assertEqual(result.stdout, '')
            self.assertIn('no parseable findings artifact', result.stderr)
            self.assertNotIn('Traceback', result.stderr)
            self.assertFalse((self.root / 'issue-title.txt').exists())

    def test_rejects_tampered_discovery_before_download(self):
        self.meta.write_text(json.dumps(dict(OBSERVATION, image='evil.example/image:latest')))
        with patch.dict(os.environ, {'FLY_READONLY_TOKEN': 'secret-canary'}), \
                patch.object(self.scan.subprocess, 'run') as docker, self.assertRaises(ValueError):
            self.scan.download(self.meta, self.root / 'image.tar')
        docker.assert_not_called()

    def test_clean_and_nonqualifying(self):
        for result in [[], [{'Target': '/binary'}], [{'Target': '/binary', 'Vulnerabilities': None}],
                       [{'Target': '/binary', 'Vulnerabilities': [dict(VULN, Severity='LOW')]}],
                       [{'Target': '/binary', 'Vulnerabilities': [dict(VULN, FixedVersion='')]}]]:
            self.assertEqual(self.render({'SchemaVersion': 2, 'Results': result}), 0)
            self.assertFalse((self.root / 'issue-title.txt').exists())

    def test_render_deterministic(self):
        critical = dict(VULN, VulnerabilityID='CVE-2026-9999', Severity='CRITICAL')
        report = copy.deepcopy(REPORT)
        report['Results'][0]['Vulnerabilities'].append(critical)
        self.assertEqual(self.render(report), 1)
        title = (self.root / 'issue-title.txt').read_text()
        self.assertIn('deployed-image', title)
        self.assertIn('CVE-2026-9999', title)
        self.assertIn('stdlib', title)
        self.assertNotIn(DIGEST, title)
        body = (self.root / 'issue-body.md').read_text()
        for text in [REF, OBSERVATION['observed_at'], '/actions/runs/42',
                     'conservative module-version matches', 'source call-path evidence',
                     'CVE-2026-1234', '1.26.1']:
            self.assertIn(text, body)
        report['Results'][0]['Vulnerabilities'].reverse()
        self.meta.write_text(json.dumps(dict(OBSERVATION, observed_at='2026-10-12T06:00:00Z')))
        self.assertEqual(self.render(report), 1)
        self.assertEqual((self.root / 'issue-title.txt').read_text(), title)

    def test_failed_artifact_write_has_no_title(self):
        original = Path.write_text
        for failed in ['issue-body.md', 'issue-title.txt.tmp']:
            def write(path, text, *args, **kwargs):
                if path.name == failed:
                    original(path, text[:8])  # Simulate a partial write.
                    raise OSError('disk write failed')
                return original(path, text, *args, **kwargs)
            with self.subTest(failed=failed), patch.object(Path, 'write_text', write), self.assertRaises(OSError):
                self.render(REPORT)
            self.assertFalse((self.root / 'issue-title.txt').exists())

    def test_invalid_scan_has_no_artifact(self):
        invalid = [None, {}, {'Results': []}, dict(REPORT, Results=None),
                   dict(REPORT, Results=[None]), dict(REPORT, Results=[{}]),
                   dict(REPORT, Results=[{'Target': '/binary', 'Vulnerabilities': {}}])]
        for vuln in [None, {}, dict(VULN, PkgName='bad\nname'),
                     dict(VULN, VulnerabilityID=None), dict(VULN, InstalledVersion=4),
                     dict(VULN, Severity='HIG'), dict(VULN, FixedVersion=None),
                     dict(VULN, FixedVersion='1.2\n3')]:
            invalid.append(dict(REPORT, Results=[{'Target': '/binary', 'Vulnerabilities': [VULN, vuln]}]))
        for report in invalid:
            with self.subTest(report=report), self.assertRaises(ValueError):
                self.render(report)
            self.assertFalse((self.root / 'issue-title.txt').exists())
        self.report.write_text(json.dumps(REPORT)[:-1])
        with self.assertRaises(ValueError):
            self.scan.render(self.report, self.meta, self.root, 'run')
        self.assertFalse((self.root / 'issue-title.txt').exists())


class IssueWorkflowTests(unittest.TestCase):
    def test_issue_filing(self):
        workflow = (HERE / 'security-scan.yml').read_text()
        script = textwrap.dedent(workflow.split('      - name: file issues\n')[1].split('        run: |\n')[1])
        generic = 'security-scan: scanner failed with no parseable findings'
        deployed = 'security-scan regression: CVE-2026-1234 in deployed-image package stdlib@v1.26.0'
        repository = deployed.replace('deployed-image', 'image')
        cases = [([repository], [], 'failure', [repository, generic]),
                 ([repository, deployed], [], 'failure', [repository, deployed]),
                 ([repository], [repository], 'failure', [generic]),
                 ([deployed], [repository], 'failure', [deployed]),
                 ([deployed], [deployed + ' extra'], 'failure', [deployed]),
                 ([deployed], [deployed], 'failure', []),
                 ([], [generic + ' extra'], 'failure', [generic]),
                 ([], [generic], 'failure', []),
                 ([repository], [], 'skipped', [repository])]
        for artifacts, existing, result, wanted in cases:
            with self.subTest(artifacts=artifacts, existing=existing, result=result), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                for i, title in enumerate(artifacts):
                    directory = root / 'artifacts' / str(i)
                    directory.mkdir(parents=True)
                    (directory / 'issue-title.txt').write_text(title + '\n')
                    (directory / 'issue-body.md').write_text('finding body')
                state = root / 'state.json'
                state.write_text(json.dumps({'existing': existing, 'created': []}))
                stub = root / 'gh'
                stub.write_text(textwrap.dedent('''\
                    #!/usr/bin/env python3
                    import json, os, sys
                    from pathlib import Path
                    state = Path(os.environ['STUB_STATE'])
                    data = json.loads(state.read_text())
                    args = sys.argv[1:]
                    if args[1] == 'list':
                        if '--jq' in args:
                            print(len(data['existing']))
                        else:
                            print(json.dumps([{'number': i, 'title': t} for i, t in enumerate(data['existing'])]))
                    else:
                        title = args[args.index('--title') + 1]
                        assert Path(args[args.index('--body-file') + 1]).is_file()
                        assert args[args.index('--label') + 1] == 'security-sensitive'
                        data['created'].append(title)
                        data['existing'].append(title)
                        state.write_text(json.dumps(data))
                    '''))
                stub.chmod(0o755)
                env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                           STUB_STATE=str(state), REPO='o/r', DEPLOYED_RESULT=result,
                           GITHUB_SERVER_URL='https://github.com', GITHUB_REPOSITORY='o/r', GITHUB_RUN_ID='42')
                for _ in range(2):
                    run = subprocess.run(['bash', '-euo', 'pipefail', '-c', script], cwd=root,
                                         env=env, text=True, capture_output=True)
                    self.assertEqual(run.returncode, 0, run.stderr)
                    self.assertEqual(json.loads(state.read_text())['created'], wanted)

    def test_workflow_isolates_credentials_and_scan_errors(self):
        workflow = (HERE / 'security-scan.yml').read_text()
        job = workflow.split('  deployed-image-scan:\n')[1].split('  govulncheck:\n')[0]
        self.assertIn("github.ref == 'refs/heads/main'", job)
        self.assertIn("github.event_name == 'schedule'", job)
        self.assertIn("github.event_name == 'workflow_dispatch'", job)
        self.assertEqual(job.count('FLY_READONLY_TOKEN:'), 2)
        scan = job.split('      - name: trivy deployed image scan\n')[1]
        self.assertNotIn('FLY_READONLY_TOKEN', scan)
        self.assertIn('input: deployed-image.tar', scan)
        self.assertIn("exit-code: '0'", scan)
        self.assertIn('severity: CRITICAL,HIGH', scan)
        self.assertIn('ignore-unfixed: true', scan)
        self.assertIn('vuln-type: os,library', scan)
        render = scan.split('      - name: render deployed findings\n')[1].split('      - name: upload')[0]
        self.assertNotIn('if: failure()', render)
        self.assertNotIn('continue-on-error', job)
        self.assertNotIn('issues: write', job)


if __name__ == '__main__':
    unittest.main()
