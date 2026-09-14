// VolatilityBlender — reference spec for the design-2 vol surface
// (GARCH anchor + live-IV skew overlay), ported to internal/volblend.
// Provided 2026-09-07 as the weighting scheme; kept verbatim as the
// cross-language reference. Two deliberate deviations in the Go port are
// documented in internal/volblend/volblend.go.
//
// Operational guidance that accompanies this spec (PDF-extraction [cite: N]
// artifacts removed, wording unchanged):
//
//   For 0DTE and Weekly Expries (T < 7 days): The model sets w_live >= 0.90.
//   Do not override this to add more GARCH. Dealer gamma flips, pinning
//   mechanics, and call walls on near-dated contracts are dictated by the
//   market prices at which market makers bought and sold contracts, not
//   historical return volatility.
//
//   For Illiquid Strikes (High Delta OTM Puts/Calls): When trading
//   low-volume single-stock options where the bid is $0.05 and ask is $0.35
//   (relative spread > 1.0), psi(k) shrinks w_live toward zero. Here, the
//   GARCH anchor prevents aberrant individual Greeks from corrupting your
//   aggregate dollar GEX profile.
//
// NOTE (port review): with LambdaT = 12 the term weight crosses 0.90 at
// T ~ 5.1 days, not 7 (phi(7/365) = 0.866). Constants are kept verbatim in
// this reference; the Go port adjusted LambdaT to ln(13/11)*365/7 =
// 8.710677271722231 (decision 2026-09-07) so the w >= 0.90 crossover lands
// exactly at 7 days per the guidance above — see
// internal/volblend/volblend.go and its TestTermWeight pin.

using System;

namespace QuantitativeOptions.Engine
{
    public static class VolatilityBlender
    {
        private const double WMin = 0.35;               // Asymptotic back-month weight on live IV
        private const double LambdaT = 12.0;             // Expiry decay parameter (annualized)
        private const double LambdaSpread = 25.0;        // Penalty coefficient for wide bid-ask spreads

        /// <summary>
        /// Computes the dynamic weight assigned to the live market IV.
        /// </summary>
        /// <param name="expiryYears">Time to expiration T in years.</param>
        /// <param name="bid">Current option bid price.</param>
        /// <param name="ask">Current option ask price.</param>
        /// <returns>Weight w in range [0.0, 1.0] for Live IV.</returns>
        public static double CalculateLiveIvWeight(double expiryYears, double bid, double ask)
        {
            if (expiryYears <= 0.0) return 1.0; // 0DTE strictly driven by live market order book

            // 1. Term Structure Component
            double phiT = WMin + (1.0 - WMin) * Math.Exp(-LambdaT * expiryYears);

            // 2. Microstructure / Quote Quality Component
            double mid = 0.5 * (bid + ask);
            if (mid <= 1e-4 || ask < bid) return phiT * 0.1; // Stale or crossed quotes shift heavily to GARCH

            double relSpread = (ask - bid) / mid;
            double psiSpread = 1.0 / (1.0 + LambdaSpread * (relSpread * relSpread));

            return Math.Clamp(phiT * psiSpread, 0.0, 1.0);
        }

        /// <summary>
        /// Combines physical GARCH forward variance with live implied variance.
        /// </summary>
        public static double BlendVariance(double liveIv, double garchVol, double expiryYears, double bid, double ask)
        {
            double wLive = CalculateLiveIvWeight(expiryYears, bid, ask);
            double varLive = liveIv * liveIv;
            double varGarch = garchVol * garchVol;

            double blendedVar = (wLive * varLive) + ((1.0 - wLive) * varGarch);
            return Math.Sqrt(Math.Max(1e-6, blendedVar));
        }

        /// <summary>
        /// Multiplicative skew formulation: Scales the market skew profile by a blended ATM anchor.
        /// </summary>
        public static double BlendSkewCarrier(double liveStrikeIv, double liveAtmIv, double garchVol,
                                              double expiryYears, double bid, double ask)
        {
            if (liveAtmIv <= 1e-4) return liveStrikeIv;

            double wLive = CalculateLiveIvWeight(expiryYears, bid, ask);
            double blendedAtmVol = (wLive * liveAtmIv) + ((1.0 - wLive) * garchVol);

            // Extract market skew multiplier
            double skewMultiplier = liveStrikeIv / liveAtmIv;

            return blendedAtmVol * skewMultiplier;
        }
    }
}
