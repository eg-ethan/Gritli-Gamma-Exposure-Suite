"""Generate hedge-module golden fixtures for the Go port.

Implements the closes pipeline — date alignment, simple daily returns, two-pass
sample variance/covariance, raw OLS beta, Pearson rho, sqrt(252) annualization,
and the hedge-sizing chain — INDEPENDENTLY of the Go code and pins the numbers
as JSON. The Go test (internal/hedge/hedge_test.go) pins ComputeStats and the
sizing chain to these values at 1e-12 rel.

Two things this fixture deliberately exercises:

  * UNALIGNED calendars: each series drops different dates (vendor gaps +
    holidays), so AlignByDate's shared-date join and the fold of returns
    across dropped dates are both covered end to end.
  * The sizing chain runs on the fixture's OWN computed beta and last closes —
    the exact numbers the Phase-2 panel will consume.

ROUNDING CONVENTION: TargetHedgeShares rounds HALF-AWAY-FROM-ZERO (Go
math.Round). Python's builtin round() is banker's rounding and must never be
used here; round_half_away below is the reference.

Run:  python docs/_verify/generate_hedge_goldens.py
Writes: core/internal/hedge/testdata/hedge_golden.json
"""
import json
import math
import os
from datetime import date, timedelta

SEED = 20260919


def lcg(seed):
    """Deterministic across Python versions — no random-module dependency."""
    state = seed
    while True:
        state = (state * 6364136223846793005 + 1442695040888963407) % (1 << 64)
        yield ((state >> 11) & ((1 << 53) - 1)) / float(1 << 53)


def gauss(rng):
    u1, u2 = next(rng), next(rng)
    return math.sqrt(-2.0 * math.log(u1)) * math.cos(2.0 * math.pi * u2)


def weekdays(start, end):
    out, d = [], start
    while d <= end:
        if d.weekday() < 5:
            out.append(d.strftime("%Y%m%d"))
        d += timedelta(days=1)
    return out


# ── mirrored computation (same op order as the Go package; changing the
# order on either side breaks the pin and both files must move together) ────

def daily_return(cur, prev):
    return (cur - prev) / prev


def mean(xs):
    s = 0.0
    for x in xs:
        s += x
    return s / float(len(xs))


def sample_variance(xs):
    m = mean(xs)
    acc = 0.0
    for x in xs:
        d = x - m
        acc += d * d
    return acc / float(len(xs) - 1)


def sample_covariance(a, b):
    ma, mb = mean(a), mean(b)
    acc = 0.0
    for i in range(len(a)):
        acc += (a[i] - ma) * (b[i] - mb)
    return acc / float(len(a) - 1)


def round_half_away(x):
    return math.floor(x + 0.5) if x >= 0 else -math.floor(-x + 0.5)


def build_case(name, beta_true, bench_sigma, idio_sigma, start_level):
    rng = lcg(SEED + (0 if name == "equity-vs-etf" else 1))
    cal = weekdays(date(2025, 9, 1), date(2026, 8, 31))

    shocks_b, shocks_e = {}, {}
    for d in cal:
        shocks_b[d] = bench_sigma * gauss(rng)
        shocks_e[d] = idio_sigma * gauss(rng)

    # per-series date sets: modular, deterministic, mostly disjoint drops
    asset_dates = [d for i, d in enumerate(cal) if i % 41 != 7]
    bench_dates = [d for i, d in enumerate(cal) if i % 59 != 13 and i % 97 != 0]

    def walk(dates, shock):
        out, p = [], start_level
        for d in dates:
            p = p * (1.0 + shock(d))
            out.append([d, p])
        return out

    asset = walk(asset_dates, lambda d: beta_true * shocks_b[d] + shocks_e[d])
    bench = walk(bench_dates, lambda d: shocks_b[d])

    # align: walk asset's dates against a bench lookup (Go AlignByDate order)
    look = {d: c for d, c in bench}
    a2, b2 = [], []
    for d, c in asset:
        if d in look:
            a2.append([d, c])
            b2.append([d, look[d]])
    dropped = len(asset) + len(bench) - 2 * len(a2)

    ra = [daily_return(a2[i][1], a2[i - 1][1]) for i in range(1, len(a2))]
    rb = [daily_return(b2[i][1], b2[i - 1][1]) for i in range(1, len(b2))]

    var_a, var_b = sample_variance(ra), sample_variance(rb)
    cov = sample_covariance(ra, rb)
    beta = cov / var_b
    rho = cov / (math.sqrt(var_a) * math.sqrt(var_b))
    vol_a = math.sqrt(var_a) * math.sqrt(252)
    vol_b = math.sqrt(var_b) * math.sqrt(252)

    legs = [
        {"delta": 0.42, "multiplier": 100, "contracts": -2},
        {"delta": -0.25, "multiplier": 100, "contracts": 3},
    ]
    shares = 120.0
    net = shares
    for leg in legs:
        net += leg["delta"] * leg["multiplier"] * float(leg["contracts"])
    s_asset, s_bench = a2[-1][1], b2[-1][1]
    dollar = net * s_asset
    hedge_val = beta * dollar
    target = round_half_away(-(hedge_val / s_bench))

    return {
        "name": name,
        "beta_constructed": beta_true,
        "asset_closes": asset,
        "bench_closes": bench,
        "expected": {
            "n_shared": len(ra),
            "dropped": dropped,
            "returns_a_head": ra[:5],
            "returns_b_head": rb[:5],
            "var_a": var_a, "var_b": var_b, "cov": cov,
            "beta": beta, "rho": rho,
            "vol_a_annual": vol_a, "vol_b_annual": vol_b,
            "hedge": {
                "shares": shares, "legs": legs,
                "spot_asset": s_asset, "spot_bench": s_bench,
                "net_delta": net, "dollar_delta": dollar,
                "hedge_value": hedge_val, "target_shares": target,
            },
        },
    }


def main():
    out = {
        "description": "Hedge-module goldens: alignment, simple daily returns, "
                       "two-pass sample stats, raw OLS beta, Pearson rho, sqrt(252) "
                       "annualization, and the sizing chain (Q rounds HALF-AWAY-FROM-ZERO, "
                       "Go math.Round — not banker's). Generated by "
                       "docs/_verify/generate_hedge_goldens.py; Go pins at 1e-12 rel.",
        "cases": [
            build_case("equity-vs-etf", 1.30, 0.011, 0.006, 380.00),
            build_case("inverse", -0.50, 0.011, 0.008, 64.00),
        ],
    }
    dest = os.path.join(os.path.dirname(__file__), os.pardir, os.pardir,
                        "core", "internal", "hedge", "testdata", "hedge_golden.json")
    dest = os.path.normpath(dest)
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    with open(dest, "w") as f:
        json.dump(out, f, indent=2)
        f.write("\n")
    print(f"wrote {dest}")
    for c in out["cases"]:
        e = c["expected"]
        print(f"  {c['name']}: N={e['n_shared']} dropped={e['dropped']} "
              f"beta={e['beta']:+.4f} rho={e['rho']:+.4f} Q={e['hedge']['target_shares']}")


if __name__ == "__main__":
    main()
