#!/usr/bin/env python3
"""R5: capture the Gateway records list (light/dark) and one SMS detail dialog.

Real taps only (adb input tap/swipe); no JS, no fixture file writes. Fails if
the records list never shows an SMS row or the detail dialog never opens, so a
missing row cannot be reported as PASS.

Usage: r5-sms-ui.py --out DIR [--serial emulator-5554]
"""
from __future__ import annotations
import argparse
import datetime as dt
import hashlib
import json
import re
import subprocess
import sys
import time
from pathlib import Path

ADB = '/home/ubuntu/gsm-webrtc-validation/android-sdk/platform-tools/adb'
PKG = 'com.callagent.gateway'
SMS_MARK = '短信'


def adb(serial, *args, timeout=45, check=True):
    result = subprocess.run([ADB, '-s', serial, *args], capture_output=True, text=True, timeout=timeout)
    if check and result.returncode != 0:
        raise RuntimeError('adb_failed:' + args[0])
    return result.stdout


def dump(serial):
    adb(serial, 'shell', 'uiautomator', 'dump', '/sdcard/r5-ui.xml', timeout=60)
    for _ in range(3):
        text = adb(serial, 'shell', 'cat', '/sdcard/r5-ui.xml')
        if text.strip().startswith('<?xml'):
            return text
        time.sleep(0.5)
    raise RuntimeError('ui_dump_unreadable')


def nodes(xml):
    out = []
    for raw in re.findall(r'<node[^>]*>', xml):
        def attr(name):
            found = re.search(name + r'="([^"]*)"', raw)
            return found.group(1) if found else ''
        bounds = [int(v) for v in re.findall(r'\d+', attr('bounds'))]
        if len(bounds) != 4:
            continue
        out.append({'text': attr('text'), 'rid': attr('resource-id'), 'class': attr('class'),
                    'clickable': attr('clickable') == 'true', 'bounds': bounds,
                    'center': ((bounds[0] + bounds[2]) // 2, (bounds[1] + bounds[3]) // 2)})
    return out


def find(items, rid=None, text_contains=None, min_y=None):
    for item in items:
        if rid and item['rid'] != rid:
            continue
        if text_contains and text_contains not in item['text']:
            continue
        if min_y is not None and item['bounds'][1] < min_y:
            continue
        return item
    return None


def tap(serial, point):
    adb(serial, 'shell', 'input', 'tap', str(point[0]), str(point[1]))


def screenshot(serial, path):
    data = subprocess.run([ADB, '-s', serial, 'exec-out', 'screencap', '-p'],
                          capture_output=True, timeout=60)
    if data.returncode != 0 or not data.stdout.startswith(b'\x89PNG'):
        raise RuntimeError('screencap_failed')
    path.write_bytes(data.stdout)


def save_set(serial, out_dir, stem, expect_detail):
    items = nodes(dump(serial))
    if expect_detail:
        title = find(items, rid='com.callagent.gateway:id/alertTitle')
        if not title or SMS_MARK not in title['text']:
            raise RuntimeError('sms_detail_dialog_missing')
        body = ' '.join(i['text'] for i in items if i['text'])
        if 'Direction' not in body and '方向' not in body:
            raise RuntimeError('sms_detail_body_missing')
    else:
        row = find(items, text_contains=SMS_MARK, min_y=190)
        if row is None:
            raise RuntimeError('sms_record_row_missing')
    screenshot(serial, out_dir / (stem + '.png'))
    (out_dir / (stem + '.xml')).write_text(dump(serial))
    return items


def open_records_tab(serial):
    adb(serial, 'shell', 'am', 'start', '-W', '-n', PKG + '/.MainActivity', timeout=60)
    time.sleep(2.0)
    items = nodes(dump(serial))
    tab = find(items, rid='com.callagent.gateway:id/navTraffic')
    if tab is None:
        raise RuntimeError('records_tab_missing')
    tap(serial, tab['center'])
    time.sleep(2.0)
    items = nodes(dump(serial))
    if find(items, text_contains=SMS_MARK, min_y=190) or find(items, rid='com.callagent.gateway:id/btnFilterAll'):
        all_filter = find(items, rid='com.callagent.gateway:id/btnFilterAll')
        if all_filter:
            tap(serial, all_filter['center'])
            time.sleep(1.0)
        return
    raise RuntimeError('records_list_missing')


def reveal_sms_row(serial):
    for attempt in range(7):
        items = nodes(dump(serial))
        for item in items:
            if SMS_MARK in item['text'] and item['bounds'][1] > 190:
                return item
        adb(serial, 'shell', 'input', 'swipe', '240', '620', '240', '260', '300')
        time.sleep(1.2)
    raise RuntimeError('sms_record_row_not_reachable')


def close_detail(serial):
    items = nodes(dump(serial))
    button = find(items, rid='android:id/button1') or find(items, text_contains='关闭')
    if button is None:
        raise RuntimeError('sms_detail_close_missing')
    tap(serial, button['center'])
    time.sleep(1.2)


def capture(serial, out_dir, stem, dark):
    current = adb(serial, 'shell', 'cmd', 'uimode', 'night').strip()
    want = 'yes' if dark else 'no'
    if want not in current:
        adb(serial, 'shell', 'cmd', 'uimode', 'night', want)
        time.sleep(4.0)
    mode = adb(serial, 'shell', 'cmd', 'uimode', 'night').strip()
    if want not in mode:
        raise RuntimeError('night_mode_not_applied:' + mode)
    open_records_tab(serial)
    row = reveal_sms_row(serial)
    save_set(serial, out_dir, 'records-' + stem, expect_detail=False)
    tap(serial, row['center'])
    time.sleep(2.0)
    save_set(serial, out_dir, 'detail-' + stem, expect_detail=True)
    close_detail(serial)
    return {'stem': stem, 'uimode': mode, 'sms_row_text': row['text']}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--serial', default='emulator-5554')
    args = parser.parse_args()
    out_dir = args.out
    if out_dir.exists():
        parser.error('output directory exists; use a fresh one')
    out_dir.mkdir(parents=True, mode=0o700)
    started = dt.datetime.now(dt.timezone.utc).isoformat()
    report = {'schema': 'gsm04-r5-sms-ui-v1', 'status': 'RUNNING', 'started_at': started,
              'serial': args.serial, 'package': PKG, 'interaction': 'adb input tap/swipe + uiautomator dump',
              'sets': []}
    try:
        for dark, stem in ((False, 'light'), (True, 'dark')):
            report['sets'].append(capture(args.serial, out_dir, stem, dark))
        adb(args.serial, 'shell', 'cmd', 'uimode', 'night', 'no')
        time.sleep(2.0)
        items = nodes(dump(args.serial))
        home = find(items, rid='com.callagent.gateway:id/navHome')
        if home:
            tap(args.serial, home['center'])
            time.sleep(1.5)
        (out_dir / 'restored.xml').write_text(dump(args.serial))
        report.update(status='PASS', finished_at=dt.datetime.now(dt.timezone.utc).isoformat(),
                      restored_uimode=adb(args.serial, 'shell', 'cmd', 'uimode', 'night').strip())
    except Exception as error:
        report.update(status='FAIL', error=type(error).__name__, error_detail=str(error)[:200],
                      finished_at=dt.datetime.now(dt.timezone.utc).isoformat())
    files = {}
    for path in sorted(out_dir.iterdir()):
        if path.is_file() and path.name != 'manifest.json':
            files[path.name] = hashlib.sha256(path.read_bytes()).hexdigest()
    report['files'] = files
    (out_dir / 'manifest.json').write_text(json.dumps(report, ensure_ascii=False, indent=2) + '\n')
    print(json.dumps({'status': report['status'], 'out': str(out_dir),
                      'sets': [s['stem'] for s in report['sets']], 'error': report.get('error')},
                     ensure_ascii=False))
    return 0 if report['status'] == 'PASS' else 1


if __name__ == '__main__':
    raise SystemExit(main())
