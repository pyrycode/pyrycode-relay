"""One read-only lifecycle poll. Run with a private JSON configuration path."""
from contextlib import contextmanager
import copy
import datetime
import fcntl
import http.client
import json
import os
from pathlib import Path
import re
import secrets
import signal
import stat
import subprocess
import sys
import tempfile

MAX_BYTES = 1048576
WINDOW = 600000
EPOCH = datetime.datetime(1970, 1, 1, tzinfo=datetime.timezone.utc)


def require(condition):
    if not condition:
        raise ValueError('invalid monitoring evidence or configuration')


def instant(value):
    require(isinstance(value, str))
    match = re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?(Z|[+-]\d\d:\d\d)', value)
    require(match is not None)
    delta = datetime.datetime.fromisoformat(match[1] + match[3].replace('Z', '+00:00')) - EPOCH
    return (delta.days * 86400 + delta.seconds) * 1000000000 + int((match[2] or '').ljust(9, '0'))


def utc(ms):
    return (EPOCH + datetime.timedelta(milliseconds=ms)).isoformat().replace('+00:00', 'Z')


def private(info, mode, kind):
    require(info.st_uid == os.getuid() and stat.S_IMODE(info.st_mode) == mode and kind(info.st_mode))


def read_private(path, directory=None):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=directory)
    with os.fdopen(fd, 'rb') as stream:
        private(os.fstat(stream.fileno()), 0o600, stat.S_ISREG)
        data = stream.read(MAX_BYTES + 1)
        require(len(data) <= MAX_BYTES)
        return data


def validate_config(config):
    require(isinstance(config, dict))
    require(isinstance(config.get('app'), str) and re.fullmatch(r'[a-z0-9][a-z0-9-]{0,62}', config['app']))
    require(isinstance(config.get('machine'), str) and re.fullmatch(r'[0-9a-f]{14,16}', config['machine']))
    for key in ('state_dir', 'token_file'):
        require(isinstance(config.get(key), str) and Path(config[key]).is_absolute())
    args = config.get('notify')
    require(isinstance(args, list) and 1 < len(args) <= 32)
    require(all(isinstance(a, str) and 0 < len(a) <= 4096 and '\0' not in a for a in args))
    require(args.count('{message_file}') == 1 and args[0] != '{message_file}')
    maintenance = config.get('maintenance')
    if maintenance is not None:
        require(isinstance(maintenance, dict))
        start, end = maintenance.get('start'), maintenance.get('end')
        require(isinstance(start, str) and isinstance(end, str) and start.endswith('Z') and end.endswith('Z'))
        require(instant(start) < instant(end))
    return config


def load_config(path):
    return validate_config(json.loads(read_private(path)))


@contextmanager
def request_deadline():
    def expired(*unused):
        raise ValueError('monitoring request timeout')
    previous = signal.signal(signal.SIGALRM, expired)
    signal.setitimer(signal.ITIMER_REAL, 20)
    try:
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


def fetch(config):
    credential = read_private(config['token_file']).decode().strip()
    require(credential and len(credential) <= 65536 and all(32 < ord(c) < 127 for c in credential))
    conn = http.client.HTTPSConnection('api.machines.dev', timeout=10)
    try:
        with request_deadline():
            conn.request('GET', f'/v1/apps/{config["app"]}/machines/{config["machine"]}',
                         headers={'Authorization': 'Bearer ' + credential})
            response = conn.getresponse()
            require(response.status == 200)  # Direct HTTPSConnection never follows redirects.
            data = response.read(MAX_BYTES + 1)
            require(len(data) <= MAX_BYTES)
            return json.loads(data)
    finally:
        conn.close()


def exit_evidence(value):
    require(isinstance(value, dict))
    flags = [value.get(k, False) for k in ('restarting', 'requested_stop', 'oom_killed')]
    require(all(type(v) is bool for v in flags))
    codes = [value.get(k, 0) for k in ('exit_code', 'guest_exit_code', 'guest_signal', 'signal')]
    require(all(type(v) is int for v in codes))
    return (instant(value.get('exited_at')), *flags, *codes)


def merge(rows):
    unique = {}
    for row in rows:
        key, at, automatic, recorded, evidence = row
        if key in unique:
            require(unique[key][:3] == row[:3] and unique[key][4] == evidence)
            recorded = max(recorded, unique[key][3])
        unique[key] = [key, at, automatic, recorded, evidence]
    return sorted(unique.values(), key=lambda row: (row[1], row[0]))


def normalize(events):
    require(isinstance(events, list) and len(events) <= 10000)
    rows = []
    for event in events:
        require(isinstance(event, dict))
        kind, recorded = event.get('type'), event.get('timestamp')
        require(isinstance(kind, str) and re.fullmatch(r'[a-z_]{1,32}', kind))
        require(type(recorded) is int and recorded > 0)
        if kind == 'exit':
            request = event.get('request')
            require(isinstance(request, dict))
            monitor = request.get('MonitorEvent', {})
            require(isinstance(monitor, dict))
            evidence = [exit_evidence(v) for v in (request.get('exit_event'), monitor.get('exit_event')) if v is not None]
            require(evidence and all(v == evidence[0] for v in evidence))
            at, restarting, planned, *_ = evidence[0]
            rows.append([f'exit|{at}', at // 1000000, restarting and not planned, recorded, list(evidence[0])])
        else:
            status, source = event.get('status', ''), event.get('source', '')
            require(all(isinstance(v, str) and len(v) <= 128 for v in (status, source)))
            key = kind + '|' + json.dumps([recorded, status, source])
            require(len(key) <= 512)
            rows.append([key, recorded, False, recorded, None])
    return merge(rows)


@contextmanager
def storage(config):
    root = Path(config['state_dir'])
    root.mkdir(mode=0o700, exist_ok=True)
    fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    lock = None
    try:
        private(os.fstat(fd), 0o700, stat.S_ISDIR)
        lock = os.open('.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600, dir_fd=fd)
        private(os.fstat(lock), 0o600, stat.S_ISREG)
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield fd
    finally:
        if lock is not None:
            os.close(lock)
        os.close(fd)


def state_name(config):
    return f'{config["app"]}-{config["machine"]}.json'


def load_state(config, directory):
    try:
        state = json.loads(read_private(state_name(config), directory))
    except FileNotFoundError:
        return {'v': 2, 'app': config['app'], 'machine': config['machine'], 'events': [],
                'coverage': None, 'last_ok': None, 'recovery': None, 'incident': None}
    require(isinstance(state, dict) and set(state) == {'v', 'app', 'machine', 'events', 'coverage', 'last_ok', 'recovery', 'incident'})
    require(type(state['v']) is int and state['v'] == 2)
    require(state.get('app') == config['app'] and state.get('machine') == config['machine'])
    for key in ('coverage', 'last_ok', 'recovery'):
        require(key in state and (state[key] is None or type(state[key]) is int and state[key] >= 0))
    require(isinstance(state.get('events'), list) and len(state['events']) <= 10001)
    for row in state['events']:
        require(isinstance(row, list) and len(row) == 5 and isinstance(row[0], str) and len(row[0]) <= 512)
        require(type(row[1]) is int and row[1] >= 0 and type(row[2]) is bool and type(row[3]) is int and row[3] >= 0)
        kind, separator, evidence = row[0].partition('|')
        require(separator and re.fullmatch(r'[a-z_]{1,32}', kind))
        if kind == 'exit':
            require(evidence.isdecimal() and int(evidence) // 1000000 == row[1])
            lifecycle = row[4]
            require(isinstance(lifecycle, list) and len(lifecycle) == 8)
            require(type(lifecycle[0]) is int and lifecycle[0] == int(evidence))
            require(all(type(v) is bool for v in lifecycle[1:4]) and all(type(v) is int for v in lifecycle[4:]))
            require(row[2] == (lifecycle[1] and not lifecycle[2]))
        else:
            anchor = json.loads(evidence)
            require(isinstance(anchor, list) and len(anchor) == 3 and type(anchor[0]) is int)
            require(anchor[0] == row[1] == row[3] and not row[2] and row[4] is None)
            require(all(isinstance(v, str) and len(v) <= 128 for v in anchor[1:]))
        require(state['last_ok'] is not None and max(row[1], row[3]) <= state['last_ok'])
    merge(state['events'])  # Reject conflicting evidence in persisted duplicate rows too.
    require('incident' in state)
    incident = state['incident']
    if incident is not None:
        require(isinstance(incident, dict) and incident.get('reason') in ('automatic restarts', 'stopped'))
        require(type(incident.get('observed')) is int and incident['observed'] >= 0)
        require(type(incident.get('count')) is int and 0 <= incident['count'] <= 10000)
        require(type(incident.get('delivered')) is bool)
        require(set(incident) == {'observed', 'reason', 'count', 'delivered'})
        require(state['last_ok'] is not None and incident['observed'] <= state['last_ok'])
    require(state['coverage'] is None if state['last_ok'] is None else state['coverage'] == state['last_ok'] - WINDOW)
    require(state['recovery'] is None or incident is not None and incident['delivered'] and state['recovery'] <= state['last_ok'])
    return state


def save_state(config, directory, state):
    data = json.dumps(state)
    require(len(data.encode()) <= MAX_BYTES)
    name = '.write-' + secrets.token_hex(12)
    try:
        fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600, dir_fd=directory)
        with os.fdopen(fd, 'w') as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(name, state_name(config), src_dir_fd=directory, dst_dir_fd=directory)
        os.fsync(directory)
    finally:
        try:
            os.unlink(name, dir_fd=directory)
        except FileNotFoundError:
            pass


def evaluate(config, prior, observation, now):
    require(isinstance(observation, dict) and observation.get('id') == config['machine'])
    status = observation.get('state')
    require(status in ('created', 'starting', 'started', 'stopping', 'stopped', 'suspending',
                       'suspended', 'replacing', 'destroying', 'destroyed', 'migrating', 'failed'))
    require(prior['last_ok'] is None or now >= prior['last_ok'])
    incoming = normalize(observation.get('events'))
    require(incoming and all(0 < row[1] <= now and row[3] <= now for row in incoming))
    cutoff = now - WINDOW
    oldest = min(event['timestamp'] for event in observation['events'])
    launched = any(row[0].startswith('launch|') and row[3] == oldest for row in incoming)
    birth = instant(observation.get('created_at')) // 1000000
    overlap = {row[0] for row in incoming} & {row[0] for row in prior['events']}
    require(oldest <= cutoff or launched and birth <= oldest <= birth + 1000 or
            overlap and prior['coverage'] is not None and prior['coverage'] <= cutoff and oldest <= prior['last_ok'])
    rows = merge(prior['events'] + incoming)
    require(len(rows) <= 10001)
    count = sum(row[2] and cutoff < row[1] <= now for row in rows)
    maintenance = config.get('maintenance')
    maintained = maintenance is not None and instant(maintenance['start']) <= now * 1000000 < instant(maintenance['end'])
    reason = 'automatic restarts' if count >= 5 else 'stopped' if status == 'stopped' and not maintained else None
    state = copy.deepcopy(prior)
    if reason and state['incident'] is None:
        state['incident'] = {'observed': now, 'reason': reason, 'count': count, 'delivered': False}
    healthy = status == 'started' and count < 5
    continuous = prior['last_ok'] is not None and 0 < now - prior['last_ok'] <= 90000
    if healthy and state['incident'] is not None and state['incident']['delivered']:
        state['recovery'] = prior['recovery'] if continuous and prior['recovery'] is not None else now
        if now - state['recovery'] >= WINDOW:
            state['incident'], state['recovery'] = None, None
    else:
        state['recovery'] = None
    older = [row for row in rows if row[1] <= cutoff]
    state['events'] = [row for row in rows if row[1] > cutoff]
    if older:
        state['events'].append(max(older, key=lambda row: row[3]))
    state['coverage'], state['last_ok'] = cutoff, now
    return state


def notify(config, incident):
    message = (f'Relay incident: app {config["app"]}; Machine {config["machine"]}\n'
               f'Observation (UTC): {utc(incident["observed"])}\n'
               f'Window (UTC): ({utc(incident["observed"] - WINDOW)}, {utc(incident["observed"])}]\n'
               f'Reason: {incident["reason"]}; automatic restart count: {incident["count"]}\n'
               'Response owner: Pyry. Investigate read-only and escalate to Juhana when operator action is needed.\n')
    path = None
    try:
        with tempfile.NamedTemporaryFile(mode='w', dir=config['state_dir'], prefix='.message-', delete=False) as stream:
            path = stream.name
            stream.write(message)
        args = [path if a == '{message_file}' else a for a in config['notify']]
        environment = {k: os.environ[k] for k in ('PATH', 'HOME', 'USER', 'LANG', 'LC_ALL') if k in os.environ}
        subprocess.run(args, env=environment, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL, timeout=15, check=True)
    finally:
        if path is not None:
            os.unlink(path)


def poll(config, now=None):
    validate_config(config)
    with storage(config) as directory:
        prior = load_state(config, directory)
        try:
            observation = fetch(config)
            if now is None:
                now = int((datetime.datetime.now(datetime.timezone.utc) - EPOCH).total_seconds() * 1000)
            state = evaluate(config, prior, observation, now)
            save_state(config, directory, state)
        except (ValueError, OSError, http.client.HTTPException, TypeError, KeyError, OverflowError, RecursionError):
            prior['recovery'] = None
            save_state(config, directory, prior)
            raise
        if state['incident'] is not None and not state['incident']['delivered']:
            notify(config, state['incident'])
            state['incident']['delivered'] = True
            save_state(config, directory, state)


def main(args=None):
    try:
        args = sys.argv[1:] if args is None else args
        require(len(args) == 1)
        poll(load_config(args[0]))
        return 0
    except (ValueError, OSError, http.client.HTTPException, subprocess.SubprocessError,
            TypeError, KeyError, OverflowError, RecursionError):
        print('relay monitoring failure; incident delivery or monitoring coverage unavailable', file=sys.stderr)
        return 2


if __name__ == '__main__':
    sys.exit(main())
