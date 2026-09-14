package market

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// LoadChainCSV reads an exported option chain CSV and returns a ChainSnapshot.
//
// Expected header (case-insensitive, exact column names):
//
//	strike,right,expiry,open_interest[,iv][,trading_class][,settlement][,exchange]
//
// `iv`, when present, is the contract's quoted implied vol (decimal, e.g.
// 0.21) and lands on Contract.IV — the volatility-skew panel's input. The
// exposure engine still prices at the GARCH term structure;
// the column is optional so exports without it replay unchanged.
//
// `trading_class` / `settlement` / `exchange` are the index
// primitives: optional, but REQUIRED on mixed SPX/SPXW exports — without a
// trading class the loader cannot segregate settlement classes and treats
// every row as one unnamed class. Settlement must be "AM", "PM", or empty.
//
// The CSV carries no conIds, so contracts get sequential negative ids
// (−1, −2, …) which can never collide with real IBKR Con_Ids. Multiplier
// defaults to 100.
func LoadChainCSV(path, ticker string, spot float64, asOfMs int64) (ChainSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return ChainSnapshot{}, fmt.Errorf("market: open chain csv: %w", err)
	}
	defer f.Close()
	return readChainCSV(f, ticker, spot, asOfMs)
}

func readChainCSV(r io.Reader, ticker string, spot float64, asOfMs int64) (ChainSnapshot, error) {
	out := ChainSnapshot{Ticker: ticker, Spot: spot, AsOfMs: asOfMs}

	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.ReuseRecord = false

	header, err := cr.Read()
	if err != nil {
		return out, fmt.Errorf("market: read csv header: %w", err)
	}
	col := make(map[string]int, len(header))
	for i, name := range header {
		col[strings.ToLower(strings.TrimSpace(name))] = i
	}
	for _, required := range []string{"strike", "right", "expiry", "open_interest"} {
		if _, ok := col[required]; !ok {
			return out, fmt.Errorf("market: csv missing required column %q (have %v)", required, header)
		}
	}
	hasIV := false
	ivIdx := 0
	if i, ok := col["iv"]; ok {
		hasIV, ivIdx = true, i
	}
	_, hasClass := col["trading_class"]
	_, hasSettle := col["settlement"]
	_, hasExch := col["exchange"]

	for row := 1; ; row++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, fmt.Errorf("market: csv row %d: %w", row, err)
		}
		c, err := parseContractRow(rec, col, hasIV, ivIdx, row)
		if err != nil {
			return out, err
		}
		if hasClass {
			if v, err := cellAt(rec, col, "trading_class", row); err == nil {
				c.TradingClass = strings.ToUpper(strings.TrimSpace(v))
			} else {
				return out, err
			}
		}
		if hasSettle {
			if v, err := cellAt(rec, col, "settlement", row); err == nil {
				c.Settlement = strings.ToUpper(strings.TrimSpace(v))
				if c.Settlement != "" && c.Settlement != SettlementAM && c.Settlement != SettlementPM {
					return out, fmt.Errorf("market: csv row %d: settlement must be AM or PM, got %q", row, v)
				}
			} else {
				return out, err
			}
		}
		if hasExch {
			if v, err := cellAt(rec, col, "exchange", row); err == nil {
				c.Exchange = strings.ToUpper(strings.TrimSpace(v))
			} else {
				return out, err
			}
		}
		c.Ticker = ticker
		// synthetic ids namespaced per ticker (see SyntheticConIDBase) so chains
		// of different underlyings never collide on the Con_Id primary key
		c.ConId = SyntheticConIDBase(ticker) - int64(len(out.Contracts)+1)
		out.Contracts = append(out.Contracts, c)
	}
	if len(out.Contracts) == 0 {
		return out, errors.New("market: chain csv contains no contracts")
	}
	if idx := IsIndexUnderlying(ticker, ""); idx {
		out.UnderlyingType = SecTypeIND
	}
	return out, nil
}

// cellAt returns the trimmed value of an optional-by-name column.
func cellAt(rec []string, col map[string]int, name string, row int) (string, error) {
	i, ok := col[name]
	if !ok {
		return "", nil
	}
	if i >= len(rec) {
		return "", fmt.Errorf("market: csv row %d: missing %s", row, name)
	}
	return rec[i], nil
}

func parseContractRow(rec []string, col map[string]int, hasIV bool, ivIdx, row int) (Contract, error) {
	at := func(name string) (string, error) {
		i, ok := col[name]
		if !ok || i >= len(rec) {
			return "", fmt.Errorf("market: csv row %d: missing %s", row, name)
		}
		return strings.TrimSpace(rec[i]), nil
	}

	strikeS, err := at("strike")
	if err != nil {
		return Contract{}, err
	}
	strike, err := strconv.ParseFloat(strikeS, 64)
	if err != nil || !finiteStrict(strike) || strike <= 0 {
		return Contract{}, fmt.Errorf("market: csv row %d: bad strike %q", row, strikeS)
	}

	rightS, err := at("right")
	if err != nil {
		return Contract{}, err
	}
	right := strings.ToUpper(rightS)
	if right != RightCall && right != RightPut {
		return Contract{}, fmt.Errorf("market: csv row %d: right must be C or P, got %q", row, rightS)
	}

	expiryS, err := at("expiry")
	if err != nil {
		return Contract{}, err
	}
	expiry := strings.ReplaceAll(expiryS, "-", "") // accept 20261016 and 2026-10-16
	if len(expiry) != 8 {
		return Contract{}, fmt.Errorf("market: csv row %d: expiry must be yyyyMMdd, got %q", row, expiryS)
	}

	oiS, err := at("open_interest")
	if err != nil {
		return Contract{}, err
	}
	oi, err := strconv.ParseFloat(oiS, 64)
	if err != nil || math.IsNaN(oi) || math.IsInf(oi, 0) || oi < 0 {
		return Contract{}, fmt.Errorf("market: csv row %d: bad open_interest %q", row, oiS)
	}

	iv := 0.0
	if hasIV && ivIdx < len(rec) && strings.TrimSpace(rec[ivIdx]) != "" {
		v, err := strconv.ParseFloat(strings.TrimSpace(rec[ivIdx]), 64)
		if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return Contract{}, fmt.Errorf("market: csv row %d: bad iv %q", row, rec[ivIdx])
		}
		iv = v
	}

	return Contract{
		Strike:       strike,
		Right:        right,
		ExpiryDate:   expiry,
		TradingClass: "",
		Multiplier:   DefaultMultiplier,
		OpenInterest: oi,
		IV:           iv,
	}, nil
}

func finiteStrict(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
