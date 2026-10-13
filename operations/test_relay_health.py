"""Hermetic lifecycle watcher checks; no Fly access or real channel posts."""
import copy
import datetime
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch

import relay_health as watch

NOW = 1791900000000
MACHINE = '7817426a000438'


def utc(ms):
    return datetime.datetime.fromtimestamp(ms / 1000, datetime.timezone.utc).isoformat().replace('+00:00', 'Z')


def event(ms, kind='start', **evidence):
    row = {'type': kind, 'timestamp': ms, 'status': 'started', 'source': 'flyd'}
    if kind == 'exit':
        row['request'] = {'exit_event': {'exited_at': utc(ms), **evidence}}
    return row


def observation(events=(), state='started', anchor=True):
    return {'id': MACHINE, 'state': state, 'created_at': utc(NOW - 86400000),
            'events': ([event(NOW - 900000)] if anchor else []) + list(events),
            'config': {'env': {'SECRET': 'remote-canary'}}}


def crashes(count=5, end=NOW):
    return [event(end - i * 60000, 'exit', restarting=True) for i in range(count)]


class WatcherTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.token = self.root / 'token'
        self.token.write_text('credential-canary')
        self.token.chmod(0o600)
        self.cfg = {'app': 'pyrycode-relay', 'machine': MACHINE,
                    'state_dir': str(self.root / 'state'), 'token_file': str(self.token),
                    'notify': [sys.executable, '-c', 'pass', '{message_file}']}
        self.config = self.root / 'config.json'
        self.config.write_text(json.dumps(self.cfg))
        self.config.chmod(0o600)

    def poll(self, obs=None, now=NOW, fail_send=False):
        with patch.object(watch, 'fetch', return_value=obs or observation()), \
                patch.object(watch, 'notify', side_effect=ValueError('child-canary') if fail_send else None) as send:
            if fail_send:
                with self.assertRaises(ValueError):
                    watch.poll(self.cfg, now)
            else:
                watch.poll(self.cfg, now)
        return send

    def state_path(self):
        return Path(self.cfg['state_dir']) / f'pyrycode-relay-{MACHINE}.json'

    def state(self):
        return json.loads(self.state_path().read_text())

    def test_threshold_and_window(self):
        for count in (0, 4, 5):
            with self.subTest(count=count):
                self.cfg['state_dir'] = str(self.root / f'case-{count}')
                send = self.poll(observation(crashes(count)))
                self.assertEqual(send.call_count, int(count == 5))
        self.cfg['state_dir'] = str(self.root / 'boundary')
        self.assertEqual(self.poll(observation(crashes(4) + [event(NOW - 600000, 'exit', restarting=True)])).call_count, 0)
        self.assertEqual(self.poll(observation(crashes(4) + [event(NOW - 599999, 'exit', restarting=True)])).call_count, 1)

    def test_first_boot(self):
        obs = observation([event(NOW - 10000, 'launch'), event(NOW, 'start')], anchor=False)
        obs['created_at'] = utc(NOW - 10000)
        self.assertEqual(self.poll(obs).call_count, 0)

    def test_exit_variants(self):
        exits = crashes()
        variants = copy.deepcopy(exits)
        for row in variants:
            row['timestamp'] += 1
            evidence = row['request'].pop('exit_event')
            evidence.update(requested_stop=False, oom_killed=False, exit_code=0)
            row['request']['MonitorEvent'] = {'exit_event': evidence}
        self.assertEqual(self.poll(observation(list(reversed(exits + variants + exits))), NOW + 1).call_count, 1)
        self.assertEqual(self.state()['incident']['count'], 5)
        for evidence in ({}, {'restarting': False}, {'restarting': True, 'requested_stop': True}):
            rows = watch.normalize([event(NOW, 'exit', **evidence)])
            self.assertFalse(rows[0][2])
        invalid = event(NOW, 'exit', restarting='true')
        with self.assertRaises(ValueError):
            watch.normalize([invalid])

    def test_late_and_maintenance(self):
        self.cfg['maintenance'] = {'start': utc(NOW - 120000), 'end': utc(NOW + 60000)}
        self.assertEqual(self.poll(observation(state='stopped'), NOW - 120000).call_count, 0)
        self.assertEqual(self.poll(observation(state='stopped')).call_count, 0)
        self.assertEqual(self.poll(observation(crashes())).call_count, 1)
        self.assertEqual(self.state()['incident']['count'], 5)
        self.cfg['state_dir'] = str(self.root / 'expired')
        self.assertEqual(self.poll(observation(state='stopped'), NOW + 60000).call_count, 1)
        self.cfg['state_dir'] = str(self.root / 'late')
        self.assertEqual(self.poll(observation(crashes(end=NOW - 610000))).call_count, 0)
        self.cfg['state_dir'] = str(self.root / 'late-maintained')
        self.assertEqual(self.poll(observation(crashes()), NOW + 120000).call_count, 1)

    def test_delivery_latch_and_restart(self):
        self.assertEqual(self.poll(observation(crashes())).call_count, 1)
        self.assertEqual(self.poll(observation(list(reversed(crashes()))), NOW + 60000).call_count, 0)
        self.assertEqual(self.poll(observation(state='stopped'), NOW + 120000).call_count, 0)
        self.assertTrue(self.state()['incident']['delivered'])

    def test_pending_retry(self):
        self.poll(observation(crashes()), fail_send=True)
        self.assertFalse(self.state()['incident']['delivered'])
        self.assertEqual(self.poll(now=NOW + 60000).call_count, 1)
        self.assertTrue(self.state()['incident']['delivered'])
        self.assertEqual(self.poll(now=NOW + 120000).call_count, 0)

    def test_rearm_and_interruptions(self):
        self.poll(observation(crashes()))
        for minute in range(1, 7):
            self.poll(now=NOW + minute * 60000)
        with patch.object(watch, 'fetch', side_effect=OSError('remote-canary')), self.assertRaises(OSError):
            watch.poll(self.cfg, NOW + 7 * 60000)
        self.assertIsNone(self.state()['recovery'])
        for minute in range(8, 18):
            self.poll(now=NOW + minute * 60000)
        self.assertIsNotNone(self.state()['incident'])
        self.poll(now=NOW + 19 * 60000)  # Missed minute resets recovery.
        for minute in range(20, 29):
            self.poll(now=NOW + minute * 60000)
        self.assertIsNotNone(self.state()['incident'])
        self.poll(now=NOW + 29 * 60000)
        self.assertIsNone(self.state()['incident'])
        self.assertEqual(self.poll(observation(crashes(end=NOW + 30 * 60000)), NOW + 30 * 60000).call_count, 1)

    def test_history_coverage(self):
        self.poll(observation(crashes(4)))
        self.poll(observation(crashes(4)[1:] + [event(NOW + 60000)], anchor=False), NOW + 60000)
        before = self.state()
        with patch.object(watch, 'fetch', return_value=observation([event(NOW + 120000)], anchor=False)), self.assertRaises(ValueError):
            watch.poll(self.cfg, NOW + 120000)
        self.assertEqual(self.state()['events'], before['events'])
        self.assertEqual(self.state()['last_ok'], before['last_ok'])
        self.cfg['state_dir'] = str(self.root / 'fresh')
        for obs in [observation([], anchor=False), observation(crashes(), anchor=False)]:
            with patch.object(watch, 'fetch', return_value=obs), self.assertRaises(ValueError):
                watch.poll(self.cfg, NOW)

    def test_schema_failure_preserves_incident(self):
        self.poll(observation(state='stopped'))
        before = self.state()['incident']
        bad = [dict(observation(), id='wrong'), dict(observation(), events=None),
               dict(observation(), state=None), observation([event(NOW + 1)]),
               observation([{'type': 'exit', 'timestamp': NOW}])]
        for obs in bad:
            with patch.object(watch, 'fetch', return_value=obs), self.assertRaises(ValueError):
                watch.poll(self.cfg, NOW)
            self.assertEqual(self.state()['incident'], before)

    def test_duplicate_exit_cannot_bridge_history_gap_or_rearm(self):
        self.poll(observation([event(NOW - 1500000)], state='stopped'), NOW - 600000)
        for minute in range(10):
            self.poll(observation(crashes(4) if minute == 9 else []), NOW - (9 - minute) * 60000)
        before = self.state()
        self.assertEqual(before['recovery'], NOW - 540000)
        duplicate = event(NOW, 'exit', restarting=True)
        duplicate['timestamp'] = NOW + 60000
        gap = observation([duplicate, event(NOW + 60001)], anchor=False)
        with patch.object(watch, 'fetch', return_value=gap), patch.object(watch, 'notify') as send:
            with self.assertRaises(ValueError):
                watch.poll(self.cfg, NOW + 60001)
        self.assertEqual(self.state(), dict(before, recovery=None))
        send.assert_not_called()
        gap['events'].append(event(NOW, 'exit', restarting=True))
        self.assertEqual(self.poll(gap, NOW + 60001).call_count, 0)
        self.assertIsNotNone(self.state()['incident'])

    def test_conflicting_exit_evidence_same_and_persisted_polls(self):
        for field, value in [('restarting', True), ('requested_stop', True), ('oom_killed', True),
                             ('exit_code', 1), ('guest_exit_code', 1), ('guest_signal', 9), ('signal', 9)]:
            for persisted in (False, True):
                with self.subTest(field=field, persisted=persisted):
                    self.cfg['state_dir'] = str(self.root / f'{field}-{persisted}')
                    self.config.write_text(json.dumps(self.cfg))
                    original = event(NOW, 'exit')
                    duplicate = event(NOW, 'exit', **{field: value})
                    if field == 'restarting':
                        duplicate['request']['exit_event']['requested_stop'] = True
                    duplicate['request']['MonitorEvent'] = {'exit_event': duplicate['request'].pop('exit_event')}
                    self.poll(observation([original] if persisted else [], state='stopped'))
                    before = self.state()
                    obs = observation([duplicate] if persisted else [original, duplicate])
                    current = datetime.datetime.fromtimestamp((NOW + 60000) / 1000, datetime.timezone.utc)
                    with patch.object(watch, 'fetch', return_value=obs), patch('builtins.print') as log, \
                            patch.object(watch.datetime, 'datetime', wraps=datetime.datetime) as clock:
                        clock.now.return_value = current
                        self.assertEqual(watch.main([str(self.config)]), 2)
                    self.assertEqual(self.state(), dict(before, recovery=None))
                    self.assertIn('relay monitoring failure', str(log.call_args_list))

    def test_equivalent_exit_evidence_survives_restart_and_validates_state(self):
        original = event(NOW, 'exit')
        self.poll(observation([original], state='stopped'))
        equivalent = event(NOW, 'exit', restarting=False, requested_stop=False, oom_killed=False,
                           exit_code=0, guest_exit_code=0, guest_signal=0, signal=0)
        equivalent['request']['MonitorEvent'] = {'exit_event': equivalent['request'].pop('exit_event')}
        self.assertEqual(self.poll(observation([equivalent]), NOW + 60000).call_count, 0)
        before = self.state()
        for bad in (dict(before, v=1, events=[row[:4] for row in before['events']]),
                    dict(before, events=[[f'exit|{NOW * 1000000}', NOW, False, NOW, [NOW * 1000000, True, False, False, 0, 0, 0, 0]]])):
            self.state_path().write_text(json.dumps(bad))
            with self.assertRaises(ValueError):
                self.poll(now=NOW + 120000)
            self.assertEqual(self.state(), bad)

    def test_private_storage_and_scope(self):
        self.poll(observation(state='stopped'))
        self.assertEqual((self.root / 'state').stat().st_mode & 0o777, 0o700)
        self.assertTrue(all(p.stat().st_mode & 0o777 == 0o600 for p in (self.root / 'state').iterdir()))
        self.assertNotIn('canary', self.state_path().read_text())
        self.cfg['machine'] = '0123456789abcd'
        self.poll(dict(observation(), id=self.cfg['machine']))
        self.assertEqual(len(list((self.root / 'state').glob('*.json'))), 2)
        self.state_path().chmod(0o644)
        self.cfg['machine'] = MACHINE
        with self.assertRaises(ValueError):
            self.poll()

    def test_lock_and_atomic_failure(self):
        self.poll(observation(state='stopped'))
        original = self.state_path().read_bytes()
        with watch.storage(self.cfg), self.assertRaises((ValueError, OSError)):
            self.poll()
        with patch.object(watch.os, 'replace', side_effect=OSError('disk-canary')), self.assertRaises(OSError):
            self.poll(now=NOW + 60000)
        self.assertEqual(self.state_path().read_bytes(), original)

    def test_private_config_and_symlink(self):
        self.assertEqual(watch.load_config(self.config)['app'], self.cfg['app'])
        self.config.chmod(0o644)
        with self.assertRaises(ValueError):
            watch.load_config(self.config)
        self.config.chmod(0o600)
        link = self.root / 'link'
        link.symlink_to(self.config)
        with self.assertRaises(OSError):
            watch.load_config(link)
        for update in ({'machine': '../bad'}, {'notify': ['sh']},
                       {'maintenance': {'start': utc(NOW), 'end': utc(NOW)}},
                       {'maintenance': {'start': '2026-10-13', 'end': utc(NOW)}}):
            self.config.write_text(json.dumps(dict(self.cfg, **update)))
            with self.assertRaises(ValueError):
                watch.load_config(self.config)

    def test_bounded_get(self):
        conn = MagicMock()
        response = conn.getresponse.return_value
        response.status, response.read.return_value = 200, json.dumps(observation()).encode()
        with patch.object(watch.http.client, 'HTTPSConnection', return_value=conn) as connect:
            self.assertEqual(watch.fetch(self.cfg)['id'], MACHINE)
        connect.assert_called_once_with('api.machines.dev', timeout=10)
        self.assertEqual(conn.request.call_args.args, ('GET', f'/v1/apps/pyrycode-relay/machines/{MACHINE}'))
        response.read.assert_called_once_with(1048577)
        conn.close.assert_called_once()
        for status, data in [(401, b'credential-canary'), (302, b'credential-canary'),
                             (200, b'{'), (200, b'x' * 1048577)]:
            response.status, response.read.return_value = status, data
            with patch.object(watch.http.client, 'HTTPSConnection', return_value=conn), self.assertRaises(ValueError):
                watch.fetch(self.cfg)

    def test_notifier_file_and_environment(self):
        self.poll(observation(crashes()))
        seen = []
        def child(args, **kwargs):
            message = Path(args[-1])
            seen.append(message)
            self.assertEqual(message.stat().st_mode & 0o777, 0o600)
            text = message.read_text()
            for value in ('pyrycode-relay', MACHINE, 'UTC', 'Pyry', '5', 'automatic restarts'):
                self.assertIn(value, text)
            self.assertNotIn('canary', text + str(args) + str(kwargs))
            self.assertEqual(kwargs['timeout'], 15)
            self.assertEqual(kwargs['stdout'], subprocess.DEVNULL)
            self.assertEqual(kwargs['stderr'], subprocess.DEVNULL)
        with patch.dict(os.environ, {'FLY_API_TOKEN': 'credential-canary', 'OTHER_SECRET': 'credential-canary'}), \
                patch.object(watch.subprocess, 'run', side_effect=child):
            watch.notify(self.cfg, self.state()['incident'])
        self.assertFalse(any(p.exists() for p in seen))

    def test_real_fake_notification_and_sanitized_cli(self):
        self.poll(observation(state='stopped'))
        self.cfg['notify'] = [sys.executable, '-c', 'import pathlib,sys; assert "Pyry" in pathlib.Path(sys.argv[1]).read_text()', '{message_file}']
        watch.notify(self.cfg, self.state()['incident'])
        with patch.object(watch, 'load_config', side_effect=ValueError('credential-canary')), patch('builtins.print') as log:
            self.assertNotEqual(watch.main([str(self.config)]), 0)
        self.assertNotIn('canary', str(log.call_args_list))
        self.assertIn('monitoring failure', str(log.call_args_list))
        result = subprocess.run([sys.executable, '-B', watch.__file__, str(self.token)], capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 2)
        self.assertNotIn('canary', result.stderr)
        self.assertNotIn('Traceback', result.stderr)

    def test_deadline_and_notification_failure_cleanup(self):
        with patch.object(watch.signal, 'signal', return_value=watch.signal.SIG_DFL) as handler, \
                patch.object(watch.signal, 'setitimer') as timer:
            with self.assertRaises(ValueError), watch.request_deadline():
                handler.call_args.args[1]()  # Simulate a peer that never completes a response.
        self.assertEqual([call.args[1] for call in timer.call_args_list], [20, 0])
        self.poll(observation(state='stopped'))
        for error in (subprocess.TimeoutExpired('credential-canary', 15),
                      subprocess.CalledProcessError(1, 'credential-canary', stderr='child-canary')):
            with patch.object(watch.subprocess, 'run', side_effect=error), self.assertRaises(subprocess.SubprocessError):
                watch.notify(self.cfg, self.state()['incident'])
            self.assertFalse(list((self.root / 'state').glob('.message-*')))

    def test_normalized_time_conflicts_and_corrupt_state(self):
        row = event(NOW, 'exit', restarting=True)
        alias = copy.deepcopy(row)
        alias['request']['exit_event']['exited_at'] = utc(NOW).replace('Z', '+00:00')
        self.assertEqual(len(watch.normalize([row, alias])), 1)
        alias['request']['exit_event']['requested_stop'] = True
        with self.assertRaises(ValueError):
            watch.normalize([row, alias])
        self.poll(observation(state='stopped'))
        corrupted = self.state()
        corrupted['incident']['delivered'] = 'true'
        self.state_path().write_text(json.dumps(corrupted))
        before = self.state_path().read_bytes()
        with self.assertRaises(ValueError):
            self.poll()
        self.assertEqual(self.state_path().read_bytes(), before)
        (self.root / 'insecure').mkdir(mode=0o755)
        self.cfg['state_dir'] = str(self.root / 'insecure')
        with self.assertRaises(ValueError):
            self.poll()

    def test_cli_remote_failure_retains_latch_and_hides_errors(self):
        self.poll(observation(state='stopped'))
        before = self.state()['incident']
        with patch.object(watch, 'fetch', side_effect=OSError('remote credential-canary')), patch('builtins.print') as log:
            self.assertEqual(watch.main([str(self.config)]), 2)
        self.assertEqual(self.state()['incident'], before)
        self.assertNotIn('canary', str(log.call_args_list))
        with patch.object(watch, 'fetch', side_effect=RecursionError('remote-canary')), patch('builtins.print') as log:
            self.assertEqual(watch.main([str(self.config)]), 2)
        self.assertEqual(self.state()['incident'], before)
        self.assertNotIn('canary', str(log.call_args_list))


if __name__ == '__main__':
    unittest.main()
