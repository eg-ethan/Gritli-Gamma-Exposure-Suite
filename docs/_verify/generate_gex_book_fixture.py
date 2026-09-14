"""Generate the cross-language GEX book fixture.

Builds a small deterministic synthetic option book and computes totals, walls,
and the zero-gamma flip with an INDEPENDENT Python implementation of the
reference algorithms (equations.md with its documented corrections: BS2002 doc
variant, central-difference vanna, spec-sign charm, calendar DTE, q = ±OI).
The Go test internal/exposure/book_fixture_test.go re-runs the same book
through exposure.ComputeSnapshot and must reproduce the totals to 1e-9 rel.

Everything is plain float arithmetic mirroring the Go operation order, so the
two implementations should agree to transcendental-ULP noise (~1e-12 rel),
far inside the 1e-9 acceptance.

Run:  python docs/_verify/generate_gex_book_fixture.py
Writes: core/internal/exposure/testdata/book_fixture.json
"""
import json
import math
import os
from datetime import datetime, timezone

SQRT2 = 1.4142135623730951
HS_FLOOR = 1e-4
HS_REL = 1e-4
HT_STEP = 1.0 / 365.0
HVOL_STEP = 1e-4
CHARM_TIME_FLOOR = 1e-5
FLIP_POINTS = 81
FLIP_SPAN = 0.20

SPOT = 500.0
R = 0.045
Q_DIV = 0.015
FLAT_VOL = 0.23
AS_OF = datetime(2026, 1, 15, 12, 0, 0, tzinfo=timezone.utc)

# (expiry, strike, right, multiplier, open_interest). Two DTE-0 contracts with
# huge OI pin the dte>=1 filtering: they must not move any output.
BOOK = []
PUT_OI_E1 = [4200, 5200, 7500, 11000, 13500, 6800, 3800, 2100, 1200]
CALL_OI_E1 = [900, 1300, 1900, 2800, 4200, 6100, 7900, 5200, 3300]
PUT_OI_E2 = [2600, 3400, 4800, 7300, 10200, 5100, 2900, 1700, 1000]
CALL_OI_E2 = [700, 1000, 1600, 2400, 3800, 5400, 6800, 4300, 2700]
_con_id = [-1000]


def con(strike, right, expiry, oi):
    _con_id[0] -= 1
    return {"con_id": _con_id[0], "strike": float(strike), "right": right,
            "expiry": expiry, "multiplier": 100.0, "open_interest": float(oi)}


for i, k in enumerate(range(460, 541, 10)):
    BOOK.append(con(k, "P", "20260122", PUT_OI_E1[i]))
    BOOK.append(con(k, "C", "20260122", CALL_OI_E1[i]))
for i, k in enumerate(range(420, 581, 20)):
    BOOK.append(con(k, "P", "20260219", PUT_OI_E2[i]))
    BOOK.append(con(k, "C", "20260219", CALL_OI_E2[i]))
# DTE-0 noise: must be invisible in every output
BOOK.append(con(490, "P", "20260115", 99999))
BOOK.append(con(510, "C", "20260115", 99999))


def norm_cdf(x):
    return 0.5 * (1.0 + math.erf(x / SQRT2))


def bs2002_call(S, K, T, r, q, sigma):
    """Doc-variant Bjerksund-Stensland 2002, mirroring solver.BjerksundStenslandCall."""
    if T == 0:
        return max(0.0, S - K)
    b = r - q
    v2 = sigma * sigma
    if b >= r:
        d1 = (math.log(S / K) + (b + 0.5 * v2) * T) / (sigma * math.sqrt(T))
        d2 = d1 - sigma * math.sqrt(T)
        return S * math.exp(-q * T) * norm_cdf(d1) - K * math.exp(-r * T) * norm_cdf(d2)
    beta = (0.5 - b / v2) + math.sqrt((b / v2 - 0.5) * (b / v2 - 0.5) + 2.0 * r / v2)
    b_inf = (beta / (beta - 1.0)) * K
    b0 = max(K, (r / (r - b)) * K)
    h_exp = -(b * T + 2.0 * sigma * math.sqrt(T)) * (b0 / (b_inf - b0))
    trigger_i = b0 + (b_inf - b0) * (1.0 - math.exp(h_exp))
    if S >= trigger_i:
        return S - K
    alpha = (trigger_i - K) * math.pow(trigger_i, -beta)

    def phi(s_in, t_in, gamma_exp, h_boundary, i_boundary):
        lambda_val = (-r + gamma_exp * b + 0.5 * gamma_exp * (gamma_exp - 1.0) * v2) * t_in
        d = -(math.log(s_in / h_boundary) + (b + (gamma_exp - 0.5) * v2) * t_in) / (sigma * math.sqrt(t_in))
        kappa = (2.0 * b) / v2 + (2.0 * gamma_exp - 1.0)
        return math.exp(lambda_val) * math.pow(s_in, gamma_exp) * (
            norm_cdf(d) - math.pow(i_boundary / s_in, kappa) * norm_cdf(
                d - (2.0 * math.log(i_boundary / s_in)) / (sigma * math.sqrt(t_in))))

    return (alpha * math.pow(S, beta)
            - alpha * phi(S, T, beta, trigger_i, trigger_i)
            + phi(S, T, 1.0, trigger_i, trigger_i)
            - phi(S, T, 1.0, K, trigger_i)
            - K * phi(S, T, 0.0, trigger_i, trigger_i)
            + K * phi(S, T, 0.0, K, trigger_i))


def price(S, K, T, r, q, sigma, right):
    if right == "C":
        return bs2002_call(S, K, T, r, q, sigma)
    return bs2002_call(K, S, T, q, r, sigma)  # put symmetry transform


def compute_greeks(S, K, T, r, q, sigma, right):
    """Mirrors solver.ComputeGreeks: central diff delta/gamma, central-diff
    vanna, spec-sign charm."""
    hs = max(HS_FLOOR, S * HS_REL)
    at = lambda s, t, vol: price(s, K, t, r, q, vol, right)
    base = at(S, T, sigma)
    up = at(S + hs, T, sigma)
    dn = at(S - hs, T, sigma)
    delta = (up - dn) / (2 * hs)
    gamma = (up - 2 * base + dn) / (hs * hs)
    delta_at = lambda vol: (at(S + hs, T, vol) - at(S - hs, T, vol)) / (2 * hs)
    vanna = (delta_at(sigma + HVOL_STEP) - delta_at(sigma - HVOL_STEP)) / (2 * HVOL_STEP)
    t_decay = max(CHARM_TIME_FLOOR, T - HT_STEP)
    delta_t = (at(S + hs, t_decay, sigma) - at(S - hs, t_decay, sigma)) / (2 * hs)
    charm = -(delta - delta_t) / HT_STEP
    return delta, gamma, vanna, charm


def dte_of(expiry, as_of):
    exp = datetime.strptime(expiry, "%Y%m%d").replace(tzinfo=timezone.utc)
    day = as_of.replace(hour=0, minute=0, second=0, microsecond=0)
    return (exp - day).days


def dealer_qty(right, oi):
    return oi if right == "C" else -oi


def total_gex_at(spot, live_contracts, vol):
    total = 0.0
    for c in live_contracts:
        dte = c["_dte"]
        T = dte / 365.0
        _, gamma, _, _ = compute_greeks(spot, c["strike"], T, R, Q_DIV, vol, c["right"])
        total += dealer_qty(c["right"], c["open_interest"]) * gamma * spot * spot * 0.01 * c["multiplier"]
    return total


def main():
    for c in BOOK:
        c["_dte"] = dte_of(c["expiry"], AS_OF)
    live = [c for c in BOOK if c["_dte"] >= 1]

    totals = {"gex": 0.0, "dex": 0.0, "vex": 0.0, "chex": 0.0}
    per_strike = {}  # strike -> [call_gex, put_gex]
    for c in live:
        T = c["_dte"] / 365.0
        delta, gamma, vanna, charm = compute_greeks(SPOT, c["strike"], T, R, Q_DIV, FLAT_VOL, c["right"])
        q = dealer_qty(c["right"], c["open_interest"])
        m = c["multiplier"]
        totals["gex"] += q * gamma * SPOT * SPOT * 0.01 * m
        totals["dex"] += q * delta * SPOT * m
        totals["vex"] += q * vanna * SPOT * 0.01 * m
        totals["chex"] += q * charm * SPOT * m
        cell = per_strike.setdefault(c["strike"], [0.0, 0.0])
        cell[0 if c["right"] == "C" else 1] += q * gamma * SPOT * SPOT * 0.01 * m

    call_wall = max(per_strike, key=lambda k: per_strike[k][0])
    put_wall = min(per_strike, key=lambda k: per_strike[k][1])

    # 81-point ±20% profile + bisection of the bracket nearest spot
    lo, hi = SPOT * (1 - FLIP_SPAN), SPOT * (1 + FLIP_SPAN)
    profile = []
    for i in range(FLIP_POINTS):
        s = lo + (hi - lo) * i / (FLIP_POINTS - 1)
        profile.append({"spot": s, "total_gex": total_gex_at(s, live, FLAT_VOL)})

    best, nearest = -1, float("inf")
    for i in range(len(profile) - 1):
        a, b = profile[i], profile[i + 1]
        if (a["total_gex"] > 0) != (b["total_gex"] > 0):
            mid = (a["spot"] + b["spot"]) / 2
            if abs(mid - SPOT) < nearest:
                nearest, best = abs(mid - SPOT), i
    has_flip = best >= 0
    flip_spot = 0.0
    if has_flip:
        a, b = profile[best]["spot"], profile[best + 1]["spot"]
        fa = total_gex_at(a, live, FLAT_VOL)
        for _ in range(100):
            m = (a + b) / 2
            if m == a or m == b:
                break
            fm = total_gex_at(m, live, FLAT_VOL)
            if fm == 0:
                a = b = m
                break
            if (fa > 0) == (fm > 0):
                a, fa = m, fm
            else:
                b = m
        flip_spot = (a + b) / 2

    out = {
        "description": "Cross-language GEX fixture: synthetic book "
                       "priced by this script's independent Python port; Go must "
                       "reproduce totals to 1e-8 rel / profile to 1e-7 of profile "
                       "scale (FD-amplified cross-library erf ULP noise), walls "
                       "exactly, flip to Brent's 1e-3 x-tolerance",
        "inputs": {
            "ticker": "FXBK",
            "spot": SPOT,
            "as_of": AS_OF.isoformat().replace("+00:00", "Z"),
            "r": R,
            "q": Q_DIV,
            "flat_vol": FLAT_VOL,
            "conventions": {
                "dte": "calendar days, UTC-midnight truncation, floor filter dte>=1",
                "time": "T = dte/365",
                "dealer_qty": "q = +OI calls, -OI puts",
                "pricer": "BS2002 doc variant; put via symmetry transform",
                "greeks": "central diff delta/gamma, central-diff vanna, spec-sign charm",
            },
            "contracts": [{k: v for k, v in c.items() if not k.startswith("_")} for c in BOOK],
        },
        "expected": {
            "totals": totals,
            "call_wall_strike": call_wall,
            "put_wall_strike": put_wall,
            "has_flip": has_flip,
            "flip_spot": flip_spot,
            "profile": profile,
        },
    }

    dest = os.path.join(os.path.dirname(__file__), os.pardir, os.pardir,
                        "core", "internal", "exposure", "testdata", "book_fixture.json")
    dest = os.path.normpath(dest)
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    with open(dest, "w") as f:
        json.dump(out, f, indent=1)
        f.write("\n")
    print(f"wrote {dest}")
    print(f"  totals: {totals}")
    print(f"  call wall {call_wall}  put wall {put_wall}  has_flip={has_flip} flip={flip_spot:.6f}")


if __name__ == "__main__":
    main()
