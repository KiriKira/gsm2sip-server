#!/usr/bin/env python3
"""Build one gsm2sip://join link (and QR code) from pairing codes of several nodes.

Each node is an independent server, so create a one-time code on every node
first, for the same role, e.g. on each server:

    gsm2sip-admin pairing-code create --owner-id <owner> --role client

Then combine them, highest priority first:

    scripts/mesh-join-link.py --role client \\
        https://home.example.lan=CODE1 https://vps.example.com=CODE2

Scanning the QR with the phone's camera opens the gsm2sip app, which pairs with
the first server (if not yet paired) and adds the others as nodes. The link
contains secrets until the codes are used or expire: do not post it anywhere.
"""
import argparse
import shutil
import subprocess
import sys
import urllib.parse

MAX_ENTRIES = 8


def build(role: str, entries: list[tuple[str, str]]) -> str:
    query = [("v", "1"), ("role", role)] + [("n", f"{url}#{code}") for url, code in entries]
    return "gsm2sip://join?" + urllib.parse.urlencode(query, quote_via=urllib.parse.quote)


def parse_entry(text: str) -> tuple[str, str]:
    url, sep, code = text.rpartition("=")
    if not sep or not url or not code:
        raise argparse.ArgumentTypeError(f"expected URL=CODE, got {text!r}")
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.query:
        raise argparse.ArgumentTypeError(f"server must be a plain https:// URL: {url!r}")
    if any(ch.isspace() for ch in code) or len(code) > 128:
        raise argparse.ArgumentTypeError("pairing code must be a single token")
    return url.rstrip("/"), code


def print_qr(link: str) -> None:
    if shutil.which("qrencode"):
        subprocess.run(["qrencode", "-t", "ansiutf8", link], check=False)
        return
    try:
        import qrcode  # type: ignore
    except ImportError:
        print("(install qrencode or the python qrcode package to print a QR code)", file=sys.stderr)
        return
    qr = qrcode.QRCode(border=2)
    qr.add_data(link)
    qr.print_ascii(invert=True)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--role", choices=["client", "gateway"], required=True,
                        help="client = main phone, gateway = old phone")
    parser.add_argument("--no-qr", action="store_true", help="print only the link")
    parser.add_argument("nodes", nargs="+", type=parse_entry, metavar="URL=CODE",
                        help="one per node, highest priority first")
    args = parser.parse_args()
    if len(args.nodes) > MAX_ENTRIES:
        parser.error(f"at most {MAX_ENTRIES} nodes per link")
    if len({url.lower() for url, _ in args.nodes}) != len(args.nodes):
        parser.error("each server may appear only once")
    link = build(args.role, args.nodes)
    print(link)
    if not args.no_qr:
        print_qr(link)
    return 0


if __name__ == "__main__":
    sys.exit(main())
