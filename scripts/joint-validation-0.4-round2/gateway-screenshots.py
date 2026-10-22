#!/usr/bin/env python3
"""Capture read-only screenshots of the already-paired Homura gateway UI.

Run this on the host that has the intended emulator connected as emulator-5554.
The script never launches, installs, clears, or unpairs the app. It only uses
resource-ID navigation, temporarily changes the records filter and system
night mode, then restores those UI settings before exiting.
"""

from __future__ import annotations

import datetime as dt
import json
import re
import shutil
import signal
import subprocess
import sys
import time
import xml.etree.ElementTree as ET
from pathlib import Path


SERIAL = "emulator-5554"
PACKAGE = "com.callagent.gateway"
UI_WAIT_SECONDS = 1.2
FILTERS = (
    ("all", "btnFilterAll"),
    ("incoming", "btnFilterIncoming"),
    ("outgoing", "btnFilterOutgoing"),
)
TABS = {
    "home": "navHome",
    "traffic": "navTraffic",
    "settings": "navSettings",
}
NIGHT_MODES = {"0": "auto", "1": "no", "2": "yes", "3": "custom"}
NIGHT_MODE_NAMES = {
    "MODE_NIGHT_AUTO": "auto",
    "MODE_NIGHT_NO": "no",
    "MODE_NIGHT_YES": "yes",
    "MODE_NIGHT_CUSTOM": "custom",
    "auto": "auto",
    "no": "no",
    "yes": "yes",
    "custom": "custom",
}


def now_utc() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="seconds")


def safe_name(value: str) -> str:
    return re.sub(r"[^a-zA-Z0-9_.-]+", "_", value).strip("_")[:80] or "capture"


class GatewayScreenshotCollector:
    def __init__(self) -> None:
        adb = shutil.which("adb")
        if not adb:
            raise RuntimeError("adb is not on PATH")
        self.adb = adb
        self.serial = SERIAL  # Deliberately fixed: never inherit or accept another device.
        self.run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        repo_root = Path(__file__).resolve().parents[1]
        self.output_dir = repo_root / "evidence" / "ui-paired" / self.run_id
        self.output_dir.mkdir(parents=True, exist_ok=False)
        self.device_xml = f"/sdcard/gateway-screenshots-{self.run_id}.xml"
        self.captures: list[dict[str, str]] = []
        self.notes: list[str] = []
        self.initial_tab: str | None = None
        self.initial_filter: str | None = None
        self.original_night_mode: str | None = None
        self.night_mode_changed = False
        self.last_tree: ET.Element | None = None
        self.screen_size: tuple[int, int] | None = None
        self.error: str | None = None

    def command(self, args: list[str], *, binary: bool = False,
                timeout: float = 30.0, check: bool = True) -> str | bytes:
        command = [self.adb, "-s", self.serial, *args]
        try:
            result = subprocess.run(command, capture_output=True, timeout=timeout, check=False)
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise RuntimeError(f"ADB command failed to run: {command!r}: {exc}") from exc
        stdout: str | bytes
        stdout = result.stdout if binary else result.stdout.decode("utf-8", errors="replace")
        stderr = result.stderr.decode("utf-8", errors="replace").strip()
        if check and result.returncode != 0:
            raise RuntimeError(f"ADB command failed ({result.returncode}): {command!r}: {stderr}")
        return stdout

    def shell(self, *args: str, check: bool = True) -> str:
        result = self.command(["shell", *args], check=check)
        assert isinstance(result, str)
        return result.strip()

    def preflight(self) -> None:
        # `adb devices` is the only command without -s; it is read-only and used
        # solely to ensure that the one explicitly authorized emulator is ready.
        listed = subprocess.run([self.adb, "devices", "-l"], capture_output=True,
                                timeout=15, check=False)
        if listed.returncode != 0:
            raise RuntimeError("adb devices -l failed: " + listed.stderr.decode(errors="replace"))
        lines = listed.stdout.decode("utf-8", errors="replace").splitlines()
        target = next((line.split() for line in lines if line.split() and line.split()[0] == SERIAL), None)
        if not target or len(target) < 2 or target[1] != "device":
            raise RuntimeError(f"{SERIAL} is not present in adb device state; found: {lines!r}")

        package_path = self.shell("pm", "path", PACKAGE, check=False)
        if "package:" not in package_path:
            raise RuntimeError(f"{PACKAGE} is not installed on {SERIAL}; refusing to launch or install it")

        activities = self.shell("dumpsys", "activity", "activities")
        resumed_lines = [line for line in activities.splitlines()
                         if "mResumedActivity" in line or "topResumedActivity" in line]
        if not any(PACKAGE in line and "MainActivity" in line for line in resumed_lines):
            raise RuntimeError(
                f"{PACKAGE}.MainActivity is not already foreground on {SERIAL}; "
                "open the paired gateway app manually, then rerun. The script will not launch it."
            )

        size_text = self.shell("wm", "size", check=False)
        sizes = re.findall(r"(\d+)x(\d+)", size_text)
        if sizes:
            self.screen_size = (int(sizes[-1][0]), int(sizes[-1][1]))
        props = {
            "android_release": self.shell("getprop", "ro.build.version.release", check=False),
            "android_sdk": self.shell("getprop", "ro.build.version.sdk", check=False),
            "device_model": self.shell("getprop", "ro.product.model", check=False),
        }
        self.notes.append("ADB actions are explicitly pinned to emulator-5554.")
        self.notes.append("The app was required to be foreground; no launch/install/data-clear was performed.")
        self.device_info = props

    @staticmethod
    def nodes(root: ET.Element) -> list[ET.Element]:
        return list(root.iter("node"))

    @staticmethod
    def resource_name(node: ET.Element) -> str:
        resource_id = node.attrib.get("resource-id", "")
        return resource_id.rsplit("/", 1)[-1]

    def find_id(self, root: ET.Element, name: str) -> ET.Element | None:
        return next((node for node in self.nodes(root) if self.resource_name(node) == name), None)

    @staticmethod
    def bounds(node: ET.Element) -> tuple[int, int, int, int] | None:
        match = re.fullmatch(r"\[(-?\d+),(-?\d+)\]\[(-?\d+),(-?\d+)\]",
                             node.attrib.get("bounds", ""))
        if not match:
            return None
        return tuple(int(part) for part in match.groups())  # type: ignore[return-value]

    def is_visible(self, node: ET.Element) -> bool:
        if node.attrib.get("visible-to-user", "true") != "true":
            return False
        bounds = self.bounds(node)
        if not bounds:
            return False
        left, top, right, bottom = bounds
        if right <= left or bottom <= top:
            return False
        if self.screen_size:
            width, height = self.screen_size
            return max(left, 0) < min(right, width) and max(top, 0) < min(bottom, height)
        return True

    def read_tree(self) -> ET.Element:
        dumped = self.command(["shell", "uiautomator", "dump", self.device_xml], timeout=20)
        if not isinstance(dumped, str):
            raise RuntimeError("UIAutomator dump returned binary data")
        raw = self.command(["exec-out", "cat", self.device_xml], timeout=20)
        if not isinstance(raw, str):
            raise RuntimeError("UIAutomator hierarchy readback returned binary data")
        try:
            root = ET.fromstring(raw)
        except ET.ParseError as exc:
            raise RuntimeError(f"UIAutomator returned invalid XML: {exc}; dump output: {dumped.strip()}") from exc
        if root.tag != "hierarchy" or not list(root):
            raise RuntimeError("UIAutomator returned an empty hierarchy")
        self.last_tree = root
        return root

    def capture(self, name: str, *, tree: ET.Element | None = None) -> ET.Element:
        time.sleep(UI_WAIT_SECONDS)
        root = tree if tree is not None else self.read_tree()
        image = self.command(["exec-out", "screencap", "-p"], binary=True, timeout=30)
        if not isinstance(image, bytes) or not image.startswith(b"\x89PNG\r\n\x1a\n"):
            raise RuntimeError(f"Screenshot capture returned invalid PNG data for {name}")
        safe = safe_name(name)
        png_path = self.output_dir / f"{safe}.png"
        xml_path = self.output_dir / f"{safe}.xml"
        png_path.write_bytes(image)
        xml_path.write_bytes(ET.tostring(root, encoding="utf-8", xml_declaration=True))
        self.captures.append({"name": name, "png": png_path.name, "uiautomator_xml": xml_path.name})
        print(f"captured {png_path}", flush=True)
        return root

    def tap(self, node: ET.Element, label: str) -> None:
        if not self.is_visible(node):
            raise RuntimeError(f"Refusing to tap an off-screen UI node ({label}): {node.attrib}")
        bounds = self.bounds(node)
        assert bounds is not None
        left, top, right, bottom = bounds
        if self.screen_size:
            width, height = self.screen_size
            left, top, right, bottom = max(0, left), max(0, top), min(width, right), min(height, bottom)
        x, y = (left + right) // 2, (top + bottom) // 2
        self.shell("input", "tap", str(x), str(y))
        time.sleep(UI_WAIT_SECONDS)

    def tap_id(self, name: str) -> ET.Element:
        root = self.read_tree()
        node = self.find_id(root, name)
        if node is None:
            raise RuntimeError(f"Expected Homura resource ID is not present: {name}")
        if not self.is_visible(node):
            raise RuntimeError(f"Expected Homura resource ID is off-screen: {name}")
        self.tap(node, name)
        return self.read_tree()

    def swipe(self, *, toward_top: bool) -> None:
        width, height = self.screen_size or (1080, 1920)
        x = width // 2
        if toward_top:
            start_y, end_y = height * 28 // 100, height * 72 // 100
        else:
            start_y, end_y = height * 78 // 100, height * 30 // 100
        self.shell("input", "swipe", str(x), str(start_y), str(x), str(end_y), "350")
        time.sleep(UI_WAIT_SECONDS)

    def scroll_to_id(self, name: str, *, toward_top: bool, attempts: int = 10) -> ET.Element:
        for _ in range(attempts + 1):
            root = self.read_tree()
            node = self.find_id(root, name)
            if node is not None and self.is_visible(node):
                return root
            self.swipe(toward_top=toward_top)
        raise RuntimeError(f"Could not bring {name} into view with bounded swipes")

    def select_tab(self, tab: str) -> ET.Element:
        if tab not in TABS:
            raise ValueError(f"Unknown tab: {tab}")
        return self.tap_id(TABS[tab])

    def detect_initial_tab(self, root: ET.Element) -> str:
        logs = self.find_id(root, "btnLogsBack")
        if logs is not None and self.is_visible(logs):
            return "logs"
        selected_tabs = []
        for tab, resource_id in TABS.items():
            node = self.find_id(root, resource_id)
            # Android's BottomNavigationView exposes its current menu item as
            # selected=true in UIAutomator; it does not expose checked=true.
            # Accept either attribute while still requiring exactly one
            # selected tab so restoration never guesses.
            if node is not None and (
                node.attrib.get("selected") == "true" or node.attrib.get("checked") == "true"
            ):
                selected_tabs.append(tab)
        if len(selected_tabs) != 1:
            raise RuntimeError(
                "Could not determine exactly one currently selected Homura tab from UIAutomator: "
                f"{selected_tabs!r}"
            )
        return selected_tabs[0]

    def verify_paired(self, root: ET.Element) -> None:
        repair = self.find_id(root, "btnControlRepair")
        paired_text = any("已配对网关" in (node.attrib.get("text", "") + " " +
                                          node.attrib.get("content-desc", ""))
                          for node in self.nodes(root))
        if repair is None or not paired_text:
            raise RuntimeError(
                "Paired-state guard failed: settings did not show the candidate UI's "
                "btnControlRepair and '已配对网关' summary. No pairing action was taken."
            )

    def read_call_filter(self, root: ET.Element) -> str:
        checked = [name for value, name in FILTERS
                   if (node := self.find_id(root, name)) is not None and node.attrib.get("checked") == "true"]
        if len(checked) != 1:
            raise RuntimeError(f"Could not determine the selected records filter: {checked!r}")
        selected_id = checked[0]
        return next(value for value, name in FILTERS if name == selected_id)

    def selected_filter_id(self, root: ET.Element) -> str | None:
        for _, name in FILTERS:
            node = self.find_id(root, name)
            if node is not None and node.attrib.get("checked") == "true":
                return name
        return None

    def mode_from_dumpsys(self) -> str:
        output = self.shell("dumpsys", "uimode")
        match = re.search(r"\bmNightMode\s*[=:]\s*([A-Za-z0-9_]+)", output)
        if not match:
            raise RuntimeError("Could not read the existing system night mode from dumpsys uimode")
        raw = match.group(1)
        mode = NIGHT_MODES.get(raw, NIGHT_MODE_NAMES.get(raw.upper(), NIGHT_MODE_NAMES.get(raw.lower())))
        if not mode:
            raise RuntimeError(f"Unrecognized existing system night mode {raw!r}; refusing to change it")
        return mode

    def set_night_mode(self, mode: str) -> None:
        self.shell("cmd", "uimode", "night", mode)
        time.sleep(2.5)
        actual = self.mode_from_dumpsys()
        if actual != mode:
            raise RuntimeError(f"System night mode stayed {actual!r} after requesting {mode!r}")

    def run(self) -> None:
        self.preflight()

        current = self.read_tree()
        self.initial_tab = self.detect_initial_tab(current)
        self.original_night_mode = self.mode_from_dumpsys()

        settings = self.select_tab("settings")
        settings = self.scroll_to_id("btnControlRepair", toward_top=True)
        self.verify_paired(settings)
        self.capture("settings-paired", tree=settings)

        settings = self.scroll_to_id("btnSimPropose", toward_top=False)
        self.capture("settings-sim", tree=settings)
        self.notes.append("SIM settings were only displayed and captured; no sync/confirm control was tapped.")

        settings = self.scroll_to_id("btnCfgLogs", toward_top=False)
        logs = self.tap_id("btnCfgLogs")
        self.capture("logs", tree=logs)
        logs_back = self.find_id(logs, "btnLogsBack")
        if logs_back is None or not self.is_visible(logs_back):
            raise RuntimeError("Logs page did not expose its expected back resource ID")
        self.tap(logs_back, "btnLogsBack")

        home = self.select_tab("home")
        self.capture("status", tree=home)

        traffic = self.select_tab("traffic")
        self.initial_filter = self.read_call_filter(traffic)
        for name, resource_id in FILTERS:
            traffic = self.tap_id(resource_id)
            if self.selected_filter_id(traffic) != resource_id:
                raise RuntimeError(f"Records filter did not select {resource_id}")
            self.capture(f"records-{name}", tree=traffic)
        original_filter_id = next(resource_id for value, resource_id in FILTERS
                                  if value == self.initial_filter)
        if self.selected_filter_id(traffic) != original_filter_id:
            traffic = self.tap_id(original_filter_id)
            if self.selected_filter_id(traffic) != original_filter_id:
                raise RuntimeError("Failed to restore the original records filter")
        self.notes.append(f"Records filter restored to {self.initial_filter!r}.")

        home = self.select_tab("home")
        if self.original_night_mode != "yes":
            self.night_mode_changed = True
            self.set_night_mode("yes")
        dark_home = self.read_tree()
        self.capture("status-dark", tree=dark_home)
        self.notes.append("System dark mode was enabled only for status-dark.png and is restored on exit.")

    def restore(self) -> None:
        # Restore system appearance first because changing it may recreate the
        # Activity. Every UI operation here remains pinned to emulator-5554.
        if self.night_mode_changed and self.original_night_mode:
            try:
                self.set_night_mode(self.original_night_mode)
                self.notes.append(f"Restored system night mode to {self.original_night_mode!r}.")
            except Exception as exc:  # Preserve the primary result, but expose cleanup failures.
                self.notes.append(f"ERROR restoring system night mode: {exc}")
                self.error = self.error or str(exc)

        if self.initial_filter:
            try:
                traffic = self.select_tab("traffic")
                wanted = next(resource_id for value, resource_id in FILTERS if value == self.initial_filter)
                if self.selected_filter_id(traffic) != wanted:
                    traffic = self.tap_id(wanted)
                if self.selected_filter_id(traffic) != wanted:
                    raise RuntimeError(f"filter remained {self.selected_filter_id(traffic)!r}")
                self.notes.append(f"Ensured records filter is restored to {self.initial_filter!r}.")
            except Exception as exc:
                self.notes.append(f"ERROR restoring records filter: {exc}")
                self.error = self.error or str(exc)

        if self.initial_tab:
            try:
                if self.initial_tab == "logs":
                    settings = self.select_tab("settings")
                    settings = self.scroll_to_id("btnCfgLogs", toward_top=False)
                    logs = self.tap_id("btnCfgLogs")
                    self.notes.append("Returned to the initial Logs page.")
                else:
                    self.select_tab(self.initial_tab)
                    self.notes.append(f"Returned to the initial {self.initial_tab!r} tab.")
            except Exception as exc:
                self.notes.append(f"ERROR restoring initial tab: {exc}")
                self.error = self.error or str(exc)

        try:
            self.shell("rm", "-f", self.device_xml, check=False)
        except Exception as exc:
            self.notes.append(f"Could not remove temporary UI hierarchy file: {exc}")

    def write_manifest(self) -> None:
        manifest = {
            "result": "capture_complete" if self.error is None else "incomplete",
            "started_at_utc": getattr(self, "started_at", now_utc()),
            "finished_at_utc": now_utc(),
            "device": {"serial": SERIAL, **getattr(self, "device_info", {})},
            "package": PACKAGE,
            "paired_state_guard": "passed" if any(item["name"] == "settings-paired" for item in self.captures)
            else "not_passed",
            "initial_tab": self.initial_tab,
            "initial_records_filter": self.initial_filter,
            "system_night_mode_before": self.original_night_mode,
            "captures": self.captures,
            "notes": self.notes,
            "error": self.error,
        }
        path = self.output_dir / "manifest.json"
        temp_path = self.output_dir / "manifest.json.tmp"
        temp_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        temp_path.replace(path)


def main() -> int:
    collector: GatewayScreenshotCollector | None = None
    try:
        collector = GatewayScreenshotCollector()
        collector.started_at = now_utc()
        for signum in (signal.SIGINT, signal.SIGTERM):
            signal.signal(signum, lambda _signum, _frame: (_ for _ in ()).throw(KeyboardInterrupt()))
        collector.run()
        print(f"Capture set: {collector.output_dir}", flush=True)
    except KeyboardInterrupt:
        if collector is not None:
            collector.error = collector.error or "interrupted; restore attempted"
        print("Interrupted; attempting to restore the original UI/system appearance.", file=sys.stderr)
    except Exception as exc:
        if collector is not None:
            collector.error = str(exc)
        print(f"ERROR: {exc}", file=sys.stderr)
    finally:
        if collector is not None:
            collector.restore()
            collector.write_manifest()
            if collector.error:
                print(f"Evidence directory: {collector.output_dir}", file=sys.stderr)
    return 1 if collector is None or collector.error else 0


if __name__ == "__main__":
    raise SystemExit(main())
