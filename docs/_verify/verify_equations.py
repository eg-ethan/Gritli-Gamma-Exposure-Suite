"""Numerical verification of equations.md reference implementation.

Ports the C# code from docs/equations.md exactly as written and compares
against a CRR binomial reference for the Bjerksund-Stensland solver, plus a
fuzz test of the Brent root finder.
"""
import math
import random

def norm_cdf(x):
    return 0.5 * (1.0 + math.erf(x / math.sqrt(2.0)))

# ---------------------------------------------------------------------------
# Doc's BjerksundStenslandCall, ported line-by-line from equations.md L200-243
# ---------------------------------------------------------------------------
def bs2002_doc(S, K, T, r, q, sigmaGarch):
    if T <= 0.0:
        return max(0.0, S - K)
    b = r - q
    v2 = sigmaGarch * sigmaGarch
    if b >= r:  # i.e. q <= 0 -> European fallback
        d1 = (math.log(S / K) + (b + 0.5 * v2) * T) / (sigmaGarch * math.sqrt(T))
        d2 = d1 - sigmaGarch * math.sqrt(T)
        return S * math.exp(-q * T) * norm_cdf(d1) - K * math.exp(-r * T) * norm_cdf(d2)

    beta = (0.5 - b / v2) + math.sqrt((b / v2 - 0.5) ** 2 + 2.0 * r / v2)
    bInf = (beta / (beta - 1.0)) * K          # <-- doc uses K here (canonical uses trigger I)
    b0 = max(K, (r / (r - b)) * K)
    h = -(b * T + 2.0 * sigmaGarch * math.sqrt(T)) * (b0 / (bInf - b0))
    triggerI = b0 + (bInf - b0) * (1.0 - math.exp(h))
    if S >= triggerI:
        return S - K
    alpha = (triggerI - K) * triggerI ** (-beta)

    def Phi(sIn, tIn, gammaExp, hBoundary, iBoundary):
        lambdaVal = (-r + gammaExp * b + 0.5 * gammaExp * (gammaExp - 1.0) * v2) * tIn
        d = -(math.log(sIn / hBoundary) + (b + (gammaExp - 0.5) * v2) * tIn) / (sigmaGarch * math.sqrt(tIn))
        kappa = (2.0 * b) / v2 + (2.0 * gammaExp - 1.0)
        return math.exp(lambdaVal) * sIn ** gammaExp * (
            norm_cdf(d) - (iBoundary / sIn) ** kappa * norm_cdf(
                d - (2.0 * math.log(iBoundary / sIn)) / (sigmaGarch * math.sqrt(tIn)))
        )

    return (alpha * S ** beta
            - alpha * Phi(S, T, beta, triggerI, triggerI)
            + Phi(S, T, 1.0, triggerI, triggerI)
            - Phi(S, T, 1.0, K, triggerI)
            - K * Phi(S, T, 0.0, triggerI, triggerI)
            + K * Phi(S, T, 0.0, K, triggerI))

# ---------------------------------------------------------------------------
# Canonical BS2002 variant: B_inf = beta/(beta-1) * I, I solved by fixed point
# ---------------------------------------------------------------------------
def bs2002_canonical(S, K, T, r, q, sigmaGarch):
    if T <= 0.0:
        return max(0.0, S - K)
    b = r - q
    v2 = sigmaGarch * sigmaGarch
    if b >= r:
        return bs2002_doc(S, K, T, r, q, sigmaGarch)
    beta = (0.5 - b / v2) + math.sqrt((b / v2 - 0.5) ** 2 + 2.0 * r / v2)
    b0 = max(K, (r / (r - b)) * K)
    # fixed-point solve of I = b0 + (beta/(beta-1)*I - b0) * (1 - exp(h))
    I = max(K, S)
    for _ in range(200):
        bInf = (beta / (beta - 1.0)) * I
        h = -(b * T + 2.0 * sigmaGarch * math.sqrt(T)) * (b0 / (bInf - b0))
        I_new = b0 + (bInf - b0) * (1.0 - math.exp(h))
        if abs(I_new - I) < 1e-12:
            I = I_new
            break
        I = I_new
    bInf = (beta / (beta - 1.0)) * I
    h = -(b * T + 2.0 * sigmaGarch * math.sqrt(T)) * (b0 / (bInf - b0))
    triggerI = b0 + (bInf - b0) * (1.0 - math.exp(h))
    if S >= triggerI:
        return S - K
    alpha = (triggerI - K) * triggerI ** (-beta)

    def Phi(sIn, tIn, gammaExp, hBoundary, iBoundary):
        lambdaVal = (-r + gammaExp * b + 0.5 * gammaExp * (gammaExp - 1.0) * v2) * tIn
        d = -(math.log(sIn / hBoundary) + (b + (gammaExp - 0.5) * v2) * tIn) / (sigmaGarch * math.sqrt(tIn))
        kappa = (2.0 * b) / v2 + (2.0 * gammaExp - 1.0)
        return math.exp(lambdaVal) * sIn ** gammaExp * (
            norm_cdf(d) - (iBoundary / sIn) ** kappa * norm_cdf(
                d - (2.0 * math.log(iBoundary / sIn)) / (sigmaGarch * math.sqrt(tIn)))
        )

    return (alpha * S ** beta
            - alpha * Phi(S, T, beta, triggerI, triggerI)
            + Phi(S, T, 1.0, triggerI, triggerI)
            - Phi(S, T, 1.0, K, triggerI)
            - K * Phi(S, T, 0.0, triggerI, triggerI)
            + K * Phi(S, T, 0.0, K, triggerI))

# ---------------------------------------------------------------------------
# CRR binomial American reference
# ---------------------------------------------------------------------------
def crr_american(S, K, T, r, q, sigma, is_call, n=4000):
    dt = T / n
    u = math.exp(sigma * math.sqrt(dt))
    d = 1.0 / u
    p = (math.exp((r - q) * dt) - d) / (u - d)
    disc = math.exp(-r * dt)
    idx = range(n + 1)
    if is_call:
        V = [max(0.0, S * u ** (n - j) * d ** j - K) for j in idx]
    else:
        V = [max(0.0, K - S * u ** (n - j) * d ** j) for j in idx]
    for i in range(n - 1, -1, -1):
        for j in range(i + 1):
            St = S * u ** (i - j) * d ** j
            cont = disc * (p * V[j] + (1 - p) * V[j + 1])
            if is_call:
                V[j] = max(cont, St - K)
            else:
                V[j] = max(cont, K - St)
    return V[0]

def doc_greeks_put_transform(S, K, T, r, q, sigmaGarch):
    """Doc's put = Call(K, S, r-(r-q), r) symmetry, as in L257."""
    return bs2002_doc(K, S, T, r - (r - q), r, sigmaGarch)

# ---------------------------------------------------------------------------
print("=== Test 1: BS2002 call vs CRR binomial American call ===")
print(f"{'case':>28} {'binomial':>10} {'doc BS02':>10} {'err':>8} {'canon BS02':>10} {'err':>8}")
cases = [
    (100, 100, 0.5, 0.05, 0.03, 0.25),
    (100, 120, 0.5, 0.05, 0.03, 0.25),
    (100, 80,  0.5, 0.05, 0.03, 0.25),
    (100, 100, 1.0, 0.08, 0.12, 0.35),
    (6000, 6000, 0.02, 0.04, 0.015, 0.15),  # SPX-like
    (100, 130, 0.08, 0.05, 0.04, 0.20),     # short-dated OTM
]
for (S, K, T, r, q, vol) in cases:
    ref = crr_american(S, K, T, r, q, vol, True, n=4000)
    doc = bs2002_doc(S, K, T, r, q, vol)
    can = bs2002_canonical(S, K, T, r, q, vol)
    print(f"S={S} K={K} T={T} r={r} q={q} v={vol}".rjust(28),
          f"{ref:10.5f} {doc:10.5f} {doc-ref:8.4f} {can:10.5f} {can-ref:8.4f}")

print()
print("=== Test 2: put via symmetry transform vs CRR binomial American put ===")
print(f"{'case':>28} {'binomial':>10} {'doc put':>10} {'err':>8}")
for (S, K, T, r, q, vol) in cases:
    ref = crr_american(S, K, T, r, q, vol, False, n=4000)
    doc = doc_greeks_put_transform(S, K, T, r, q, vol)
    print(f"S={S} K={K} T={T} r={r} q={q} v={vol}".rjust(28),
          f"{ref:10.5f} {doc:10.5f} {doc-ref:8.4f}")

# ---------------------------------------------------------------------------
print()
print("=== Test 3: Brent root finder (doc port) fuzz vs bisection ===")

def brent_doc(f, a, b, tol=1e-3, maxIter=100):
    fa, fb = f(a), f(b)
    if fa * fb > 0.0:
        return float('nan')
    if abs(fa) < abs(fb):
        a, b = b, a
        fa, fb = fb, fa
    c, fc = a, fa
    mflag = True
    d = 0.0
    for _ in range(maxIter):
        if abs(fa - fc) > 1e-12 and abs(fb - fc) > 1e-12:
            s = (a * fb * fc) / ((fa - fb) * (fa - fc)) + \
                (b * fa * fc) / ((fb - fa) * (fb - fc)) + \
                (c * fa * fb) / ((fc - fa) * (fc - fb))
        else:
            s = b - fb * (b - a) / (fb - fa)
        cond1 = (s < (3.0 * a + b) / 4.0 and s > b) or (s > (3.0 * a + b) / 4.0 and s < b)
        cond2 = mflag and abs(s - b) >= abs(b - c) / 2.0
        cond3 = (not mflag) and abs(s - b) >= abs(c - d) / 2.0
        cond4 = mflag and abs(b - c) < tol
        cond5 = (not mflag) and abs(c - d) < tol
        if cond1 or cond2 or cond3 or cond4 or cond5:
            s = (a + b) / 2.0
            mflag = True
        else:
            mflag = False
        fs = f(s)
        d = c
        c, fc = b, fb
        if fa * fs < 0.0:
            b, fb = s, fs
        else:
            a, fa = s, fs
        if abs(fa) < abs(fb):
            a, b = b, a
            fa, fb = fb, fa
        if abs(b - a) < tol or abs(fb) < 1e-9:
            return b
    return b

def bisect(f, a, b, tol=1e-6):
    fa, fb = f(a), f(b)
    if fa * fb > 0:
        return float('nan')
    for _ in range(200):
        m = (a + b) / 2
        fm = f(m)
        if fa * fm <= 0:
            b = m
        else:
            a, fa = m, fm
        if abs(b - a) < tol:
            break
    return (a + b) / 2

random.seed(42)
fails = 0
trials = 4000
worst = 0.0
for t in range(trials):
    # random cubic with roots inside the bracket, plus wiggle
    r1, r2, r3 = sorted(random.uniform(0.0, 1.0) for _ in range(3))
    f = lambda x, r1=r1, r2=r2, r3=r3: (x - r1) * (x - r2) * (x - r3)
    a, b = 0.0, 1.0
    got = brent_doc(f, a, b)
    want = bisect(f, a, b, 1e-9)
    # accept any true root (there are 3)
    ok = (not math.isnan(got)) and min(abs(got - r1), abs(got - r2), abs(got - r3)) < 5e-3
    if not ok:
        fails += 1
        if fails <= 5:
            print(f"  FAIL: roots=({r1:.4f},{r2:.4f},{r3:.4f}) brent={got:.6f} bisect={want:.6f}")
print(f"cubic fuzz: {fails}/{trials} failures")

fails2 = 0
for t in range(trials):
    # smooth oscillatory GEX-like function: sign changes inside bracket
    k1 = random.uniform(2.0, 9.0)
    k2 = random.uniform(2.0, 9.0)
    f = lambda x, k1=k1, k2=k2: math.sin(k1 * x) * math.exp(-x / 3) + 0.3 * math.sin(k2 * x) - 0.1
    a, b = 0.0, 3.0
    fa, fb = f(a), f(b)
    if fa * fb > 0:
        continue
    got = brent_doc(f, a, b)
    if math.isnan(got) or abs(f(got)) > 1e-2:
        fails2 += 1
        if fails2 <= 5:
            print(f"  FAIL osc: brent={got:.6f} f(brent)={f(got) if not math.isnan(got) else float('nan'):.2e}")
print(f"oscillatory fuzz: {fails2} failures (of those with valid bracket)")

# ---------------------------------------------------------------------------
print()
print("=== Test 4: charm sign, code vs doc spec ===")
# doc spec:  Charm = -(Delta(T) - Delta(T-h))/h   (i.e. +dDelta/dt calendar time)
# code:      charm = -(deltaT - delta)/ht with deltaT = Delta(T-h) -> +(Delta(T)-Delta(T-h))/h
S, K, T, r, q, vol = 100.0, 100.0, 0.5, 0.05, 0.03, 0.25

def delta_doc(S, K, T, r, q, vol, put=False):
    hs = max(1e-4, S * 1e-4)
    if not put:
        pu = bs2002_doc(S + hs, K, T, r, q, vol)
        pd = bs2002_doc(S - hs, K, T, r, q, vol)
    else:
        pu = doc_greeks_put_transform(S + hs, K, T, r, q, vol)
        pd = doc_greeks_put_transform(S - hs, K, T, r, q, vol)
    return (pu - pd) / (2 * hs)

ht = 1.0 / 365.0
d_now = delta_doc(S, K, T, r, q, vol)
d_later = delta_doc(S, K, T - ht, r, q, vol)  # T-h = one day closer to expiry
code_charm = -(d_later - d_now) / ht          # exactly as the C# does
spec_charm = -(d_now - d_later) / ht          # exactly as the doc math says
print(f"delta(T)={d_now:.6f}  delta(T-h)={d_later:.6f}")
print(f"code charm  = {code_charm:+.4f}")
print(f"spec charm  = {spec_charm:+.4f}   (they are exact opposites: code = -spec)")
print(f"reference: calendar-time dDelta/dt for a call should be NEGATIVE here "
      f"(delta decays toward 0 with theta): code {code_charm:+.4f} vs spec {spec_charm:+.4f}")
