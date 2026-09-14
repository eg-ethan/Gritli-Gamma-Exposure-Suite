package fit

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"gexcore/internal/garch"
)

// FromArchJSON imports a fit produced by the Python `arch` package. arch
// works in PERCENT units (returns × 100); garch.Params are DECIMAL DAILY —
// the conversions are ω/10000, h/10000, ε/100, with α/γ/β dimensionless and
// unchanged (garch/params.go package comment, the documented unit trap).
//
// Expected JSON schema (export from the arch job with this shape):
//
//	{
//	  "ticker": "SPY",
//	  "dist": "t",
//	  "n_obs": 2500,
//	  "units": "percent",                  // REQUIRED: anything else is refused
//	  "params": {"omega": 0.0352, "alpha": [0.082], "gamma": [0.064], "beta": [0.895]},
//	  "last_state": {"h": 0.81, "eps": -0.42}   // optional, percent units
//	}
//
// The "units" guard exists because a percent-units ω written straight into
// GARCH_Parameters would poison every downstream σ̄(T) by four orders of
// magnitude — silently. Refusing the write is the diagnosable failure.
func FromArchJSON(r io.Reader, now time.Time) (garch.Params, garch.State, error) {
	var doc struct {
		Ticker string `json:"ticker"`
		Dist   string `json:"dist"`
		NObs   int    `json:"n_obs"`
		Units  string `json:"units"`
		Params struct {
			Omega float64   `json:"omega"`
			Alpha []float64 `json:"alpha"`
			Gamma []float64 `json:"gamma"`
			Beta  []float64 `json:"beta"`
		} `json:"params"`
		LastState *struct {
			H   float64 `json:"h"`
			Eps float64 `json:"eps"`
		} `json:"last_state"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return garch.Params{}, garch.State{}, fmt.Errorf("fit: arch json: %w", err)
	}
	if doc.Units != "percent" {
		return garch.Params{}, garch.State{},
			fmt.Errorf("fit: arch json: units %q — must declare \"percent\" (the ω/10000 conversion is not optional)", doc.Units)
	}
	if doc.Ticker == "" {
		return garch.Params{}, garch.State{}, fmt.Errorf("fit: arch json: ticker is required")
	}
	if len(doc.Params.Alpha) != 1 || len(doc.Params.Beta) != 1 || len(doc.Params.Gamma) > 1 {
		return garch.Params{}, garch.State{}, fmt.Errorf("fit: arch json: expected GJR(1,1,1) parameter shapes")
	}
	if doc.Dist == "" {
		doc.Dist = "t"
	}

	p := garch.Params{
		Ticker:        doc.Ticker,
		Omega:         doc.Params.Omega / 10000, // percent² → decimal²
		Alpha:         doc.Params.Alpha[0],
		GammaLeverage: 0,
		Beta:          doc.Params.Beta[0],
		Dist:          doc.Dist,
		NObs:          doc.NObs,
		FittedAtMs:    now.UnixMilli(),
	}
	if len(doc.Params.Gamma) == 1 {
		p.GammaLeverage = doc.Params.Gamma[0]
	}
	if err := p.Validate(); err != nil {
		return garch.Params{}, garch.State{}, fmt.Errorf("fit: arch json params invalid after conversion: %w", err)
	}

	st := garch.State{Ticker: doc.Ticker}
	if doc.LastState != nil {
		st.LastH = doc.LastState.H / 10000 // percent² → decimal²
		st.LastEps = doc.LastState.Eps / 100
		st.UpdatedAtMs = now.UnixMilli()
	}
	return p, st, nil
}
