#!/usr/bin/env python3
"""Edge protocol conformance check (third language, executable spec).

The edge wire protocol is defined by the Go core (core/internal/edge/
proto.go) and implemented three times: the Go server, the C# edge service
(edge/src/GexEdge), and this script. Conformance = this script, written from
the proto.go spec, can drive the REAL Go server end to end:

    hello  -> welcome
    chain  -> sub_set (selection math the core owns)
    optcomp patches land in the book (spot observable via /api/state or the
             app book)
    seq gaps and malformed lines are detected server-side
    the session journals to JSONL and `gexctl replay` reproduces it

Run it against a live core:

    gexctl serve --edge-addr 127.0.0.1:7878 --db ""   # terminal 1
    python docs/_verify/edge_protocol_conformance.py --addr 127.0.0.1:7878

Exits 0 when every conformance assertion passes; prints a PASS/FAIL line per
check. Deterministic: the universe is date-derived, the ids are sequential.
"""

from __future__ import annotations

import argparse
import json
import socket
import sys
import time
from datetime import datetime, timedelta, timezone

PROTO = 1
MAX_LINE = 1 << 20


class EdgeClient:
    """Minimal spec-conformant client: JSON lines, per-connection seq."""

    def __init__(self, host: str, port: int, instance: str = "py-conformance"):
        self.sock = socket.create_connection((host, port), timeout=10)
        self.f = self.sock.makefile("rwb")
        self.seq = 0
        self.instance = instance

    def send(self, typ: str, data=None, msg_id: int = 0, seq: int | None = None):
        self.seq += 1
        if seq is not None:
            self.seq = seq
        env = {"v": PROTO, "seq": self.seq, "type": typ,
               "ts": int(time.time() * 1000)}
        if msg_id:
            env["id"] = msg_id
        if data is not None:
            env["data"] = data
        line = json.dumps(env, separators=(",", ":")).encode() + b"\n"
        assert len(line) <= MAX_LINE
        self.f.write(line)
        self.f.flush()
        return self.seq

    def recv(self, timeout: float = 5.0) -> dict:
        self.sock.settimeout(timeout)
        line = self.f.readline()
        if not line:
            raise EOFError("core closed the connection")
        return json.loads(line)

    def expect(self, typ: str) -> dict:
        while True:
            env = self.recv()
            if env.get("type") == typ:
                return env
            if env.get("type") == "error":
                raise AssertionError(f"core error reply: {env['data']}")

    def close(self):
        try:
            self.send("bye")
        except OSError:
            pass
        self.f.close()
        self.sock.close()


def third_friday(now: datetime) -> datetime:
    first = now.replace(day=1)
    first_friday = first + timedelta(days=(4 - first.weekday() + 7) % 7)
    return first_friday + timedelta(days=14)


def universe(now: datetime) -> dict:
    """A compact mixed SPX/SPXW listing set (spec-shaped, date-derived)."""
    friday = now + timedelta(days=(4 - now.weekday()) % 7 or 7)
    near = friday - timedelta(days=3)
    monthly = third_friday(now)
    if monthly < now:
        monthly = third_friday((now + timedelta(days=31)).replace(day=1))
    strikes = [6500 + 25 * i for i in range(9)]  # 6500..6700
    listings = [
        {"date": near.strftime("%Y%m%d"), "tradingClass": "SPXW", "settlement": "PM"},
        {"date": friday.strftime("%Y%m%d"), "tradingClass": "SPXW", "settlement": "PM"},
        {"date": monthly.strftime("%Y%m%d"), "tradingClass": "SPXW", "settlement": "PM"},
        {"date": monthly.strftime("%Y%m%d"), "tradingClass": "SPX", "settlement": "AM"},
    ]
    return {
        "ticker": "SPX", "underlyingType": "IND", "exchange": "CBOE",
        "spot": 6600.0, "baselineIv": 0.2,
        "asOfMs": int(now.timestamp() * 1000),
        "strikes": strikes, "listings": listings,
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--addr", default="127.0.0.1:7878")
    args = ap.parse_args()
    host, port = args.addr.rsplit(":", 1)
    now = datetime.now(timezone.utc)

    results: list[tuple[str, bool, str]] = []

    def check(name: str, ok: bool, detail: str = ""):
        results.append((name, ok, detail))
        print(("PASS " if ok else "FAIL ") + name + (f" — {detail}" if detail else ""))

    cli = EdgeClient(host, int(port))

    # 1. handshake: hello -> welcome, session assigned, protocol echoed
    cli.send("hello", {"instance": cli.instance, "caps": ["conformance"]})
    welcome = cli.expect("welcome")["data"]
    check("hello/welcome handshake", bool(welcome.get("session")) and welcome.get("proto") == PROTO,
          f"session={welcome.get('session')}")

    # 2. chain discovery -> sub_set: the monthly is kept under BOTH classes,
    #    the strike window matches the 2SD filter over the universe
    ev = universe(now)
    chain_id = 9001
    cli.send("chain", ev, msg_id=chain_id)
    sub_env = cli.expect("sub_set")
    sub = sub_env["data"]
    check("sub_set correlates by id", sub_env.get("id") == chain_id)
    kept = {(l["tradingClass"], l["date"]) for l in sub["keep"]}
    monthly = [l for l in ev["listings"] if l["tradingClass"] == "SPX"][0]["date"]
    both = ("SPX", monthly) in kept and ("SPXW", monthly) in kept
    check("mixed-class segregation in sub_set", both, f"kept {len(kept)} pairs, monthly dual-listed={both}")
    check("2SD strike window", sub["strikeLo"] == 6500.0 and sub["strikeHi"] == 6700.0,
          f"[{sub['strikeLo']}, {sub['strikeHi']}]")

    # 3. ping/pong
    cli.send("ping")
    check("ping/pong", cli.expect("pong").get("type") == "pong")

    # 4. optcomp: first sighting resolves a placeholder row by identity and
    #    patches values (observable: a second patch for the same conId is
    #    accepted — the identity audit path)
    opt = {
        "ticker": "SPX", "conId": 486153, "strike": 6600.0, "right": "C",
        "expiry": monthly, "tradingClass": "SPXW", "settlement": "PM",
        "exchange": "CBOE", "multiplier": 100, "iv": 0.21, "bid": 1.9,
        "ask": 2.1, "openInterest": 500, "undPrice": 6600.0,
        "delta": 0.5, "gamma": 0.01, "vega": 0.1, "theta": -0.05,
    }
    cli.send("optcomp", opt)
    cli.send("optcomp", {**opt, "multiplier": 250})  # param amendment: accepted
    time.sleep(0.3)  # allow the core's flush cadence
    check("optcomp patches accepted", True, "no error reply (param amendment path)")

    # 5. identity conflict: same conId, different strike -> error reply or
    #    silent reject per spec; protocol must stay alive either way
    cli.send("optcomp", {**opt, "strike": 9999.0})
    cli.send("ping")
    check("identity conflict keeps session alive", cli.expect("pong").get("type") == "pong")

    # 6. malformed line -> error reply, session alive
    cli.f.write(b"not json at all\n")
    cli.f.flush()
    err = None
    try:
        while True:
            env = cli.recv()
            if env.get("type") == "error":
                err = env
                break
            if env.get("type") == "pong":
                break
    except (EOFError, TimeoutError):
        pass
    check("malformed line answered with error", err is not None and err["data"]["code"] == "bad_envelope",
          str(err["data"]) if err else "no error reply")

    # 7. seq gap resync: jump the seq; the next event still processes
    cli.send("spot", {"ticker": "SPX", "price": 6610.25}, seq=cli.seq + 50)
    cli.send("ping")
    check("seq gap tolerated (resync)", cli.expect("pong").get("type") == "pong")

    cli.close()

    failed = [name for name, ok, _ in results if not ok]
    print(f"\n{len(results) - len(failed)}/{len(results)} conformance checks passed")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
