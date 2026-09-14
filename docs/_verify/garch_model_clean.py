"""
GARCH-Powered Volatility and Dealer Exposure Tracking Module
Replaces Black-Scholes and Monte Carlo models with conditional heteroskedasticity.
"""

import numpy as np
import pandas as pd
from scipy.stats import norm, t as student_t
from arch import arch_model


class GarchVolatilityEngine:
    def __init__(self, p: int = 1, o: int = 1, q: int = 1, dist: str = 'StudentsT'):
        """
        GJR-GARCH(p, o, q) specification.
        o=1 introduces asymmetry (gamma) to model the leverage effect.
        """
        self.p = p
        self.o = o
        self.q = q
        self.dist = dist
        self.model_fit = None
        self.diurnal_factors = None

    def fit_daily_gjr_garch(self, returns: pd.Series) -> dict:
        """
        Fits GJR-GARCH(1,1,1) on daily returns to obtain long-run physical variance parameters.
        Returns are expressed in percentage (returns * 100).
        """
        rescaled_ret = returns * 100.0
        am = arch_model(
            rescaled_ret, 
            mean='Constant', 
            vol='GARCH', 
            p=self.p, 
            o=self.o, 
            q=self.q, 
            dist=self.dist
        )
        self.model_fit = am.fit(disp='off')
        
        omega = self.model_fit.params['omega']
        alpha = self.model_fit.params['alpha[1]']
        gamma = self.model_fit.params['gamma[1]']
        beta = self.model_fit.params['beta[1]']
        
        # Annualized unconditional long-run volatility: sqrt(252 * omega / (1 - alpha - beta - 0.5*gamma))
        persistence = alpha + beta + 0.5 * gamma
        unconditional_var = omega / (1.0 - persistence) if persistence < 1.0 else np.nan
        unconditional_vol = (np.sqrt(unconditional_var * 252.0)) / 100.0

        # Most recent conditional volatility (annualized)
        current_cond_vol = (np.sqrt(self.model_fit.conditional_volatility.iloc[-1]**2 * 252.0)) / 100.0

        return {
            "omega": omega,
            "alpha": alpha,
            "gamma_leverage": gamma,
            "beta": beta,
            "persistence": persistence,
            "current_annualized_vol": current_cond_vol,
            "unconditional_vol": unconditional_vol,
            "fit_summary": self.model_fit
        }

    def forecast_forward_vol(self, horizon_days: int = 5) -> float:
        """
        Generates multi-step physical volatility forecast for VRP calculation.
        """
        if self.model_fit is None:
            raise ValueError("Fit daily GJR-GARCH model before calling forecast.")
        
        forecasts = self.model_fit.forecast(horizon=horizon_days)
        # Sum conditional variances across the horizon, rescale from percent back to decimal
        cumulative_variance = (forecasts.variance.iloc[-1].sum() / 10000.0)
        annualized_forecast = np.sqrt(cumulative_variance * (252.0 / horizon_days))
        return annualized_forecast

    def compute_diurnal_pattern(self, intraday_bars: pd.DataFrame) -> pd.Series:
        """
        Computes intraday U-shape seasonality across time-of-day buckets.
        intraday_bars must contain ['timestamp', 'return'].
        """
        df = intraday_bars.copy()
        df['time_of_day'] = df['timestamp'].dt.time
        df['sq_return'] = df['return'] ** 2
        
        # Mean squared return per intraday bucket relative to global mean
        bucket_variance = df.groupby('time_of_day')['sq_return'].mean()
        self.diurnal_factors = bucket_variance / bucket_variance.mean()
        return self.diurnal_factors

    def get_intraday_conditional_vol(self, last_bar_return: float, 
                                     prior_h: float, 
                                     time_of_day, 
                                     omega_intra=1e-5, 
                                     alpha_intra=0.08, 
                                     beta_intra=0.88) -> tuple:
        """
        Steps intraday GARCH(1,1) forward by 1 interval, adjusting for diurnal seasonality.
        """
        s_i = self.diurnal_factors.get(time_of_day, 1.0) if self.diurnal_factors is not None else 1.0
        
        # De-seasonalize prior innovation
        u_prev = last_bar_return / np.sqrt(max(s_i, 1e-6))
        
        # Step stochastic variance
        h_current = omega_intra + alpha_intra * (u_prev**2) + beta_intra * prior_h
        
        # Total composite intraday conditional standard deviation
        sigma_composite = np.sqrt(s_i * h_current)
        return sigma_composite, h_current


class GarchDealerAnalytics:
    def __init__(self, garch_engine: GarchVolatilityEngine, contract_multiplier: int = 100):
        self.garch = garch_engine
        self.multiplier = contract_multiplier

    def bulk_volume_classification(self, delta_price: float, volume: float, 
                                    sigma_garch: float) -> tuple:
        """
        Classifies bar volume into buyer/seller flows normalized by GARCH conditional sigma.
        """
        if sigma_garch <= 1e-8:
            z_score = 0.0
        else:
            z_score = delta_price / sigma_garch

        p_buy = norm.cdf(z_score)
        v_buy = volume * p_buy
        v_sell = volume * (1.0 - p_buy)
        net_signed_flow = v_buy - v_sell
        return v_buy, v_sell, net_signed_flow

    def compute_variance_risk_premium(self, atm_iv: float, forecast_horizon_days: int = 5) -> dict:
        """
        Calculates Variance Risk Premium against forward GARCH expectation.
        """
        garch_physical_vol = self.garch.forecast_forward_vol(horizon_days=forecast_horizon_days)
        vrp = atm_iv - garch_physical_vol
        
        if vrp > 0.03:
            regime = "HARVESTABLE_VRP (Implied Overpriced / Mean-Reversion Favored)"
        elif vrp < -0.01:
            regime = "NEGATIVE_VRP (Physical Vol Elevated / Shock / Tail Expansion)"
        else:
            regime = "NEUTRAL_VRP"

        return {
            "atm_iv": atm_iv,
            "garch_physical_vol": garch_physical_vol,
            "vrp_spread": vrp,
            "regime": regime
        }

    def compute_gex_augmented_band(self, delta: float, gamma: float, spot: float, 
                                   sigma_garch: float, lambda_cost: float = 0.0005, 
                                   gamma_a: float = 1e-6) -> dict:
        """
        Computes Whalley-Wilmott discrete delta hedge bands dynamically scaled by GARCH vol.
        """
        denom = 2.0 * gamma_a * max(sigma_garch**2, 1e-6)
        width = ((3.0 * lambda_cost * spot * (gamma**2)) / denom) ** (1.0 / 3.0)
        return {
            "lower_delta": delta - width,
            "upper_delta": delta + width,
            "band_width": width
        }

    def calculate_dollar_gex(self, chain_df: pd.DataFrame, spot: float) -> pd.DataFrame:
        """
        Calculates per-strike dollar GEX using estimated/market option gammas.
        chain_df must contain: ['strike', 'gamma', 'dealer_inventory', 'option_type']
        """
        df = chain_df.copy()
        # Dollar GEX = DealerInventory * Gamma * S^2 * 0.01 * Multiplier
        df['dollar_gex'] = (
            df['dealer_inventory'] * df['gamma'] * (spot ** 2) * 0.01 * self.multiplier
        )
        return df

    def evaluate_market_state(self, spot: float, chain_df: pd.DataFrame, 
                               atm_iv: float, sigma_garch_intraday: float) -> dict:
        """
        Synthesizes dealer gamma exposure, walls, GARCH physical vol, and VRP into a trading read.
        """
        gex_df = self.calculate_dollar_gex(chain_df, spot)
        total_gex = gex_df['dollar_gex'].sum()
        
        calls = gex_df[gex_df['option_type'] == 'C']
        puts = gex_df[gex_df['option_type'] == 'P']
        
        call_wall = calls.groupby('strike')['dollar_gex'].sum().idxmax()
        put_wall = puts.groupby('strike')['dollar_gex'].sum().idxmin()
        
        vrp_data = self.compute_variance_risk_premium(atm_iv)
        
        # Characterize regime based on dealer exposure and conditional volatility
        if total_gex > 0:
            gamma_regime = "POSITIVE_GAMMA (Dealers Suppress Realized Vol)"
            actionable_context = (
                f"Fading extreme moves into Call Wall ({call_wall}) and Put Wall ({put_wall}). "
                "Order flow expected to mean-revert."
            )
        else:
            gamma_regime = "NEGATIVE_GAMMA (Dealers Accelerate Realized Vol)"
            actionable_context = (
                "Breakout mode active. Hedging flows reinforce directional momentum. "
                "Avoid fading moves; trade with order flow."
            )

        return {
            "spot": spot,
            "total_dollar_gex": total_gex,
            "call_wall": call_wall,
            "put_wall": put_wall,
            "gamma_regime": gamma_regime,
            "vrp_analysis": vrp_data,
            "actionable_context": actionable_context
        }


# =====================================================================
# VERIFICATION & WORKFLOW EXAMPLE
# =====================================================================
if __name__ == "__main__":
    np.random.seed(42)
    
    # 1. Simulate 500 days of log returns with leverage effect
    days = 500
    sim_returns = np.zeros(days)
    sigma = 0.015
    for t in range(1, days):
        shock = np.random.normal(0, 1)
        sim_returns[t] = sigma * shock
        # Manual leverage: downside returns induce higher next-day volatility
        asym = 0.15 if shock < 0 else 0.0
        sigma = np.sqrt(1e-5 + 0.05 * (sim_returns[t]**2) + asym * (sim_returns[t]**2) + 0.85 * (sigma**2))
    
    ret_series = pd.Series(sim_returns)
    
    # 2. Fit GJR-GARCH model
    garch_core = GarchVolatilityEngine()
    garch_fit = garch_core.fit_daily_gjr_garch(ret_series)
    print("=== GJR-GARCH Model Fit ===")
    print(f"Alpha (Shock Sensitivity): {garch_fit['alpha']:.4f}")
    print(f"Gamma (Leverage Asymmetry): {garch_fit['gamma_leverage']:.4f}")
    print(f"Beta (Persistence):        {garch_fit['beta']:.4f}")
    print(f"Current Annualized Vol:    {garch_fit['current_annualized_vol'] * 100:.2f}%\n")
    
    # 3. Setup mock option chain with dealer inventories
    spot_price = 450.00
    strikes = np.linspace(430, 470, 9)
    mock_data = []
    
    for k in strikes:
        # Calls: Dealer assumed short (-OI) from retail speculation, or long (+OI)
        mock_data.append({
            'strike': k,
            'option_type': 'C',
            'gamma': max(0.001, 0.05 - 0.002 * abs(spot_price - k)),
            'dealer_inventory': 2500 if k >= spot_price else -1500
        })
        # Puts: Customer protective hedging implies dealer short puts (-OI)
        mock_data.append({
            'strike': k,
            'option_type': 'P',
            'gamma': max(0.001, 0.05 - 0.002 * abs(spot_price - k)),
            'dealer_inventory': -3000 if k <= spot_price else 1000
        })
        
    chain_dataframe = pd.DataFrame(mock_data)
    
    # 4. Run Analytics Coordinator
    dealer_analytics = GarchDealerAnalytics(garch_core)
    state = dealer_analytics.evaluate_market_state(
        spot=spot_price,
        chain_df=chain_dataframe,
        atm_iv=0.21,  # 21% market IV
        sigma_garch_intraday=0.0012
    )
    
    print("=== Dealer Exposure & GARCH Market Read ===")
    print(f"Underlying Spot:        ${state['spot']:.2f}")
    print(f"Aggregate Dollar GEX:   ${state['total_dollar_gex']:,.0f}")
    print(f"Gamma Regime:           {state['gamma_regime']}")
    print(f"Call Wall (Resistance): ${state['call_wall']:.2f}")
    print(f"Put Wall (Support):     ${state['put_wall']:.2f}")
    print(f"Forward GARCH Vol Est:  {state['vrp_analysis']['garch_physical_vol'] * 100:.2f}%")
    print(f"ATM Implied Vol:        {state['vrp_analysis']['atm_iv'] * 100:.2f}%")
    print(f"VRP Regime:             {state['vrp_analysis']['regime']}")
    print(f"Actionable Context:     {state['actionable_context']}")