package store

import (
	"path/filepath"
	"testing"

	"gexcore/internal/hedge"
)

// TestDailyClosesRoundTrip: upsert → read back ascending; re-loading a
// partial file replaces only the dates it carries (extends history, never
// truncates); tickers stay isolated.
func TestDailyClosesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "closes.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rows := []hedge.Close{
		{Date: "20260105", Close: 100.5},
		{Date: "20260106", Close: 101.25},
		{Date: "20260107", Close: 99.75},
	}
	if err := s.UpsertDailyCloses("PANW", rows); err != nil {
		t.Fatal(err)
	}
	s.FlushAndWait()

	// partial reload: one restated date + one new date
	if err := s.UpsertDailyCloses("PANW", []hedge.Close{
		{Date: "20260106", Close: 101.5}, // restatement replaces
		{Date: "20260108", Close: 100.0}, // new row extends
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDailyCloses("CIBR", []hedge.Close{{Date: "20260106", Close: 30.25}}); err != nil {
		t.Fatal(err)
	}
	s.FlushAndWait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// reopen: migration (v6) must be a no-op and the rows must survive
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	got, ok, err := s2.DailyCloses("PANW")
	if err != nil || !ok {
		t.Fatalf("load PANW: ok=%v err=%v", ok, err)
	}
	want := []hedge.Close{
		{Date: "20260105", Close: 100.5},
		{Date: "20260106", Close: 101.5},
		{Date: "20260107", Close: 99.75},
		{Date: "20260108", Close: 100.0},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d closes %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("close[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	cibr, ok, err := s2.DailyCloses("CIBR")
	if err != nil || !ok || len(cibr) != 1 || cibr[0].Close != 30.25 {
		t.Fatalf("CIBR isolation broken: %v ok=%v err=%v", cibr, ok, err)
	}
	if _, ok, err := s2.DailyCloses("NOPE"); err != nil || ok {
		t.Fatalf("unknown ticker must be ok=false without error, got ok=%v err=%v", ok, err)
	}

	// --replace path: delete one ticker, the other survives
	if err := s2.DeleteDailyCloses("PANW"); err != nil {
		t.Fatal(err)
	}
	s2.FlushAndWait()
	if _, ok, err := s2.DailyCloses("PANW"); err != nil || ok {
		t.Fatalf("deleted ticker must be ok=false without error, got ok=%v err=%v", ok, err)
	}
	if cibr, ok, err := s2.DailyCloses("CIBR"); err != nil || !ok || len(cibr) != 1 {
		t.Fatalf("CIBR must survive a PANW delete: %v ok=%v err=%v", cibr, ok, err)
	}
}
