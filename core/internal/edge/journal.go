package edge

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// The journal is the forensic record of an edge session: every wire line as
// received (and every core reply), stamped with the receive time. Because the
// journal stores the raw wire format, `gexctl replay` re-feeds it through the
// exact live dispatch path — a production anomaly recorded today is a
// deterministic reproduction tomorrow, no TWS required.
//
// Record shape (JSONL):
//
//	{"dir":"in","recvMs":1757236800123,"line":"{\"v\":1,\"seq\":4,...}"}
type journalRecord struct {
	Dir    string `json:"dir"` // "in" | "out"
	RecvMs int64  `json:"recvMs"`
	Line   string `json:"line"`
}

// Journal is an append-mode JSONL writer. Each record is flushed on write:
// a journal that loses its tail on crash is a forensic instrument with a
// hole in it.
type Journal struct {
	f *os.File
}

// OpenJournal opens (or creates) a journal for append.
func OpenJournal(path string) (*Journal, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("edge: open journal: %w", err)
	}
	return &Journal{f: f}, nil
}

// Record appends one wire line. dir is "in" or "out".
func (j *Journal) Record(dir string, line []byte, at time.Time) error {
	if j == nil {
		return nil
	}
	b, err := json.Marshal(journalRecord{Dir: dir, RecvMs: at.UnixMilli(), Line: string(line)})
	if err != nil {
		return err
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("edge: journal write: %w", err)
	}
	return nil
}

// Close flushes and closes the underlying file.
func (j *Journal) Close() error {
	if j == nil {
		return nil
	}
	return j.f.Close()
}

// JournalEntry is one decoded journal record.
type JournalEntry struct {
	Dir    string
	RecvMs int64
	Line   []byte
}

// ReadJournal loads a journal for replay/inspection.
func ReadJournal(path string) ([]JournalEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("edge: read journal: %w", err)
	}
	defer f.Close()

	var out []JournalEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 128*1024), MaxLineBytes*2)
	for ln := 1; sc.Scan(); ln++ {
		var rec journalRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("edge: journal line %d: %w", ln, err)
		}
		out = append(out, JournalEntry{Dir: rec.Dir, RecvMs: rec.RecvMs, Line: []byte(rec.Line)})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("edge: read journal: %w", err)
	}
	return out, nil
}
