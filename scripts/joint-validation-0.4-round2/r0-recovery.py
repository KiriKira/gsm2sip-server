"""R0/R0b/R0c: verify Homura b6dfcea releases calls left unknown.

R0   legacy call released within 60 s of launch, then outgoing + incoming work.
R0b  force-stop mid-call, cellular still up -> must NOT release; cancel -> release.
R0c  force-stop mid-call, cancel first, then launch -> release within 60 s.
"""
import datetime, importlib.util, json, pathlib, subprocess, sys, time

root = pathlib.Path('/home/ubuntu/gsm-webrtc-validation/joint-0.4')
ev = root / 'evidence'
private = root / 'private'
spec = importlib.util.spec_from_file_location('core', root / 'scripts' / 'core-calls.py')
core = importlib.util.module_from_spec(spec)
spec.loader.exec_module(core)
d = core.d

LEGACY = '8aab9419-fe70-4350-95e3-6abc295bb38b'
PACKAGE = 'com.callagent.gateway'


def utc():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def launch_activity(timeout=60):
    """Launch the visible app the way a user does; returns timing only."""
    started = time.monotonic()
    out = subprocess.run([d.ADB[0], '-s', d.SERIALS['gateway'], 'shell', 'am', 'start', '-W', '-n',
                          PACKAGE + '/.MainActivity'], capture_output=True, text=True, timeout=timeout)
    return {'rc': out.returncode, 'seconds': round(time.monotonic() - started, 1),
            'stdout': out.stdout.strip()[-300:], 'stderr': out.stderr.strip()[-200:]}


def app_start(role='gateway', timeout=120):
    started = time.monotonic()
    out = subprocess.run([sys.executable, str(root / 'scripts' / 'apps.py'), 'start', role],
                         capture_output=True, text=True, timeout=timeout)
    return {'rc': out.returncode, 'seconds': round(time.monotonic() - started, 1),
            'stdout': out.stdout.strip()[-400:], 'stderr': out.stderr.strip()[-400:]}


def modem_list():
    return subprocess.check_output([d.ADB[0], '-s', d.SERIALS['gateway'], 'emu', 'gsm', 'list'],
                                   text=True, timeout=20).strip()


def force_stop():
    return subprocess.check_output([d.ADB[0], '-s', d.SERIALS['gateway'], 'shell', 'am', 'force-stop', PACKAGE],
                                   text=True, timeout=30).strip()


def empty_terminals(call):
    """rtc_control_terminals must no longer carry the call."""
    ledger_dir = private / ('r0-db-' + call[:8])
    ledger_dir.mkdir(parents=True, exist_ok=True)
    return ledger_dir


def db_snapshot(call):
    """ledger row + slot + queued terminals straight from the app database copy."""
    import io, sqlite3, tarfile
    data = subprocess.check_output([d.ADB[0], '-s', d.SERIALS['gateway'], 'exec-out', 'run-as', PACKAGE,
                                    'tar', '-cf', '-', 'databases'])
    dest = empty_terminals(call)
    with tarfile.open(fileobj=io.BytesIO(data)) as t:
        t.extractall(dest, filter='data')
    con = sqlite3.connect(dest / 'databases' / 'gateway-data.db')
    rows = con.execute('select call_id,state,direction from call_ledger where call_id=?',
                       (call,)).fetchall()
    slot = con.execute('select count(*) from rtc_cellular_slot where call_id=?', (call,)).fetchone()[0]
    try:
        terms = con.execute('select count(*) from rtc_control_terminals where call_id=?', (call,)).fetchone()[0]
    except sqlite3.OperationalError:
        terms = None
    con.close()
    return {'rows': rows, 'slot': slot, 'terminals': terms}


def server_row(call, node):
    return core.sql("select state,coalesce(reason,''),exists(select 1 from gateway_call_slots s where s.call_id=c.call_id) "
                    "from call_sessions c where call_id='" + call + "'", node)


def released(call, node):
    """Return evidence when the call counts as released everywhere."""
    snap = db_snapshot(call)
    row = server_row(call, node)
    rows = snap['rows']
    ok = (len(rows) == 1 and rows[0][1] == 'failed' and snap['slot'] == 0 and not snap['terminals']
          and row.startswith('ended|cellular_ended|f'))
    return ok, {'db': snap, 'server': row}


def log_tail(offset):
    log = private / 'gateway-logcat.txt'
    if not log.exists():
        return ''
    with log.open('rb') as f:
        f.seek(offset)
        return f.read().decode(errors='replace')


def wait_release(call, node, offset, timeout=60):
    started = time.monotonic()
    last = None
    while time.monotonic() - started < timeout:
        ok, last = released(call, node)
        if ok:
            tail = log_tail(offset)
            line = next((l.strip() for l in tail.splitlines()
                         if 'unknown_call_released' in l and call in l), '')
            return {'seconds': round(time.monotonic() - started, 1), 'ok': True, 'state': last, 'log': line}
        time.sleep(2)
    return {'seconds': round(time.monotonic() - started, 1), 'ok': False, 'state': last,
            'log': next((l.strip() for l in log_tail(offset).splitlines() if 'unknown_call_released' in l), '')}


def guard_no_release(call, node, offset, seconds=60):
    """Nothing may be released while the cellular call is still up."""
    started = time.monotonic()
    seen = []
    while time.monotonic() - started < seconds:
        seen.append({'t': round(time.monotonic() - started, 1), 'db': db_snapshot(call), 'server': server_row(call, node),
                     'modem': modem_list()})
        time.sleep(10)
    tail = log_tail(offset)
    released_lines = [l.strip() for l in tail.splitlines() if call in l and 'unknown_call_released' in l]
    return {'seconds': round(time.monotonic() - started, 1), 'released_lines': released_lines, 'observations': seen}


def quick(mode):
    out = subprocess.run([sys.executable, str(root / 'scripts' / 'core-calls.py'), mode],
                         capture_output=True, text=True, timeout=600)
    path = ev / (mode + '.json')
    data = json.loads(path.read_text()) if path.exists() else {}
    return {'mode': mode, 'rc': out.returncode, 'status': data.get('status'), 'error': data.get('error'),
            'audio_seconds': data.get('audio_seconds'), 'actual_telecom_calls': data.get('actual_telecom_calls'),
            'active_node': data.get('active_node'), 'server_terminal': data.get('server_terminal'),
            'gateway_ledger': data.get('gateway_ledger')}


def follow_up():
    return {'outgoing': quick('quick-outgoing-r0'), 'incoming': quick('quick-incoming-r0')}


def case(name):
    c = core.Case(name)
    return c


def r0(c):
    c.result['legacy_call'] = LEGACY
    node = 'a'
    c.result['server_before'] = server_row(LEGACY, 'a')
    c.result['db_before'] = db_snapshot(LEGACY)
    log = private / 'gateway-logcat.txt'
    offset = log.stat().st_size if log.exists() else 0
    c.result['gateway_log_offset'] = offset
    started = time.monotonic()
    c.result['launch'] = launch_activity()
    c.result['release'] = wait_release(LEGACY, node, offset, timeout=max(10, 60 - (time.monotonic() - started)))
    c.result['elapsed_from_launch'] = round(time.monotonic() - started, 1)
    c.result['app_start'] = app_start()
    assert c.result['release']['ok'], 'legacy_call_not_released: ' + json.dumps(c.result['release']['state'])
    assert 'unknown_call_released' in c.result['release']['log'], 'release_log_missing'
    c.save()
    c.result['follow_up'] = follow_up()
    assert c.result['follow_up']['outgoing']['status'] == 'PASS', 'outgoing_after_release_failed'
    assert c.result['follow_up']['incoming']['status'] == 'PASS', 'incoming_after_release_failed'
    c.pass_()


def reproduce(c, name, cancel_first):
    """One answered call, force-stop, then either cancel first (R0c) or keep it (R0b)."""
    c.incoming()
    core.cmd('host', 'answer')
    c.connected()
    call = c.result['call_id']
    node = core.server_node(c.result.get('active_node'))
    c.result['repro_node'] = node
    c.result['modem_before_force_stop'] = modem_list()
    log = private / 'gateway-logcat.txt'
    offset = log.stat().st_size if log.exists() else 0
    c.result['gateway_log_offset'] = offset
    c.result['force_stop_stdout'] = force_stop()
    stopped = time.monotonic()
    # Wait for the server to give up on the call control (unknown|control_expired).
    server_states = []
    while time.monotonic() - stopped < 60:
        server_states.append({'t': round(time.monotonic() - stopped, 1), 'server': server_row(call, node),
                              'modem': modem_list()})
        if server_states[-1]['server'].startswith('unknown'):
            break
        time.sleep(5)
    c.result['while_stopped'] = server_states
    assert server_states[-1]['server'].startswith('unknown'), 'server_never_marked_call_unknown'
    if cancel_first:
        subprocess.check_output([d.ADB[0], '-s', d.SERIALS['gateway'], 'emu', 'gsm', 'cancel', '+155****0200'],
                                text=True, timeout=20)
        c.result['modem_after_cancel'] = modem_list()
        time.sleep(2)
    started = time.monotonic()
    c.result['launch'] = launch_activity()
    if cancel_first:
        c.result['release'] = wait_release(call, node, offset, timeout=max(10, 60 - (time.monotonic() - started)))
        c.result['elapsed_from_launch'] = round(time.monotonic() - started, 1)
        c.result['app_start'] = app_start()
        assert c.result['release']['ok'], 'not_released_after_cancel_first: ' + json.dumps(c.result['release']['state'])
        assert 'unknown_call_released' in c.result['release']['log'], 'release_log_missing'
    else:
        c.result['app_start'] = app_start()
        c.result['guard'] = guard_no_release(call, node, offset, seconds=60)
        assert not c.result['guard']['released_lines'], 'released_while_cellular_call_alive'
        guard_ok, guard_state = released(call, node)
        assert not guard_ok, 'released_while_cellular_call_alive'
        c.result['guard_state_during'] = guard_state
        subprocess.check_output([d.ADB[0], '-s', d.SERIALS['gateway'], 'emu', 'gsm', 'cancel', '+155****0200'],
                                text=True, timeout=20)
        c.result['modem_after_cancel'] = modem_list()
        c.result['release'] = wait_release(call, node, offset, timeout=60)
        assert c.result['release']['ok'], 'not_released_after_cancel: ' + json.dumps(c.result['release']['state'])
        assert 'unknown_call_released' in c.result['release']['log'], 'release_log_missing'
    c.save()
    c.result['follow_up'] = follow_up()
    assert c.result['follow_up']['outgoing']['status'] == 'PASS', 'outgoing_after_release_failed'
    assert c.result['follow_up']['incoming']['status'] == 'PASS', 'incoming_after_release_failed'
    c.pass_()


def main(mode):
    c = case({'r0': 'r0-legacy-unknown-call-release',
              'r0b': 'r0b-force-stop-cellular-held',
              'r0c': 'r0c-force-stop-cancel-first'}[mode])
    try:
        if mode == 'r0':
            r0(c)
        else:
            reproduce(c, mode, cancel_first=(mode == 'r0c'))
    except Exception as e:
        c.result.update(status='FAIL', error=str(e))
        c.save()
        raise


if __name__ == '__main__':
    main(sys.argv[1])
