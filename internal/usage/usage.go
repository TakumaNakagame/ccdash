// Package usage totals token usage — and an API-price estimate of it — from
// Claude Code transcripts. Every assistant entry carries message.usage; the
// same message is written several times while it streams, so entries are
// deduplicated by message id. Files are read incrementally: a Scanner
// remembers how far it got in each transcript and only reads what was
// appended since.
//
// The cost is what the tokens would cost on the Claude API at list price.
// On a subscription plan it is not a bill — callers label it as an estimate.
package usage

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Totals is a token count and its estimated cost.
type Totals struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheWrite int64   `json:"cache_write"`
	CacheRead  int64   `json:"cache_read"`
	Cost       float64 `json:"cost"`     // USD at API list price; 0 for unknown models
	Messages   int     `json:"messages"` // distinct assistant messages
}

func (t *Totals) add(o Totals) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheWrite += o.CacheWrite
	t.CacheRead += o.CacheRead
	t.Cost += o.Cost
	t.Messages += o.Messages
}

// Tokens is every token counted, cache included.
func (t Totals) Tokens() int64 { return t.Input + t.Output + t.CacheWrite + t.CacheRead }

// price is $ per million tokens.
type price struct{ in, out, cacheRead float64 }

// prices by model id prefix (longest prefix wins). List prices from the
// Claude API model table; cacheRead 0 means "10% of input".
var prices = map[string]price{
	"claude-fable-5-1":  {10, 50, 0.25},
	"claude-fable-5":    {10, 50, 0},
	"claude-mythos-5":   {10, 50, 0.25},
	"claude-opus-5-5":   {4, 20, 0.20},
	"claude-opus-5":     {5, 25, 0},
	"claude-opus-4":     {5, 25, 0},
	"claude-sonnet-5-5": {2, 10, 0.20},
	"claude-sonnet-5":   {2, 10, 0},
	"claude-sonnet-4-6": {3, 15, 0},
	"claude-sonnet-4":   {3, 15, 0},
	"claude-haiku-4-5":  {1, 5, 0},
}

func priceOf(model string) (price, bool) {
	best, bestLen := price{}, 0
	for k, p := range prices {
		if strings.HasPrefix(model, k) && len(k) > bestLen {
			best, bestLen = p, len(k)
		}
	}
	return best, bestLen > 0
}

type rawUsage struct {
	Input         int64 `json:"input_tokens"`
	Output        int64 `json:"output_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	CacheDetail   struct {
		OneHour  int64 `json:"ephemeral_1h_input_tokens"`
		FiveMins int64 `json:"ephemeral_5m_input_tokens"`
	} `json:"cache_creation"`
}

// cost prices one message: cache writes at 1.25x input (5 min) or 2x
// (1 hour), reads at the listed cache price or 10% of input.
func cost(model string, u rawUsage) float64 {
	p, ok := priceOf(model)
	if !ok {
		return 0
	}
	read := p.cacheRead
	if read == 0 {
		read = p.in / 10
	}
	oneHour, fiveMin := u.CacheDetail.OneHour, u.CacheDetail.FiveMins
	if oneHour+fiveMin == 0 {
		fiveMin = u.CacheCreation
	}
	usd := float64(u.Input)*p.in + float64(u.Output)*p.out +
		float64(fiveMin)*p.in*1.25 + float64(oneHour)*p.in*2 + float64(u.CacheRead)*read
	return usd / 1e6
}

// File is the running total of one transcript.
type File struct {
	Total   Totals            `json:"total"`
	ByDay   map[string]Totals `json:"by_day"`   // local date YYYY-MM-DD
	ByModel map[string]Totals `json:"by_model"` // model id
	Last    time.Time         `json:"last,omitzero"`
}

type fileState struct {
	offset int64
	size   int64
	mtime  time.Time
	seen   map[string]bool
	data   File
}

// Scanner caches per-file progress; safe for concurrent use.
type Scanner struct {
	mu    sync.Mutex
	files map[string]*fileState
}

// NewScanner returns an empty Scanner.
func NewScanner() *Scanner { return &Scanner{files: map[string]*fileState{}} }

// Default is the process-wide scanner the collector and TUI share.
var Default = NewScanner()

// Scan returns the totals of path, reading only what was appended since the
// last call (a shrunk or replaced file is re-read from the start).
func (s *Scanner) Scan(path string) (File, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return File{}, err
	}
	s.mu.Lock()
	st := s.files[path]
	if st == nil || fi.Size() < st.size {
		st = &fileState{seen: map[string]bool{}, data: File{ByDay: map[string]Totals{}, ByModel: map[string]Totals{}}}
		s.files[path] = st
	}
	if fi.Size() == st.size && fi.ModTime().Equal(st.mtime) {
		out := st.data.clone()
		s.mu.Unlock()
		return out, nil
	}
	s.mu.Unlock()

	f, err := os.Open(path)
	if err != nil {
		return File{}, err
	}
	defer f.Close()

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := f.Seek(st.offset, io.SeekStart); err != nil {
		return File{}, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	off := st.offset
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a partial last line is read again next time
		}
		off += int64(len(line))
		st.consume(line)
	}
	st.offset, st.size, st.mtime = off, fi.Size(), fi.ModTime()
	return st.data.clone(), nil
}

func (st *fileState) consume(line []byte) {
	if !strings.Contains(string(line), `"usage"`) {
		return
	}
	var e struct {
		Type      string    `json:"type"`
		Timestamp time.Time `json:"timestamp"`
		Message   struct {
			ID    string   `json:"id"`
			Model string   `json:"model"`
			Usage rawUsage `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.Message.ID == "" || st.seen[e.Message.ID] {
		return
	}
	if e.Message.Model == "<synthetic>" {
		return
	}
	st.seen[e.Message.ID] = true
	u := e.Message.Usage
	t := Totals{Input: u.Input, Output: u.Output, CacheWrite: u.CacheCreation, CacheRead: u.CacheRead, Cost: cost(e.Message.Model, u), Messages: 1}
	st.data.Total.add(t)
	day := e.Timestamp.Local().Format("2006-01-02")
	d := st.data.ByDay[day]
	d.add(t)
	st.data.ByDay[day] = d
	m := st.data.ByModel[e.Message.Model]
	m.add(t)
	st.data.ByModel[e.Message.Model] = m
	if e.Timestamp.After(st.data.Last) {
		st.data.Last = e.Timestamp
	}
}

func (f File) clone() File {
	out := File{Total: f.Total, Last: f.Last, ByDay: map[string]Totals{}, ByModel: map[string]Totals{}}
	for k, v := range f.ByDay {
		out.ByDay[k] = v
	}
	for k, v := range f.ByModel {
		out.ByModel[k] = v
	}
	return out
}

// Merge adds b into a (by day and by model too).
func Merge(a *File, b File) {
	if a.ByDay == nil {
		a.ByDay = map[string]Totals{}
	}
	if a.ByModel == nil {
		a.ByModel = map[string]Totals{}
	}
	a.Total.add(b.Total)
	for k, v := range b.ByDay {
		t := a.ByDay[k]
		t.add(v)
		a.ByDay[k] = t
	}
	for k, v := range b.ByModel {
		t := a.ByModel[k]
		t.add(v)
		a.ByModel[k] = t
	}
	if b.Last.After(a.Last) {
		a.Last = b.Last
	}
}

// SessionFiles is a session transcript plus its subagents' transcripts
// (<transcript without .jsonl>/subagents/agent-*.jsonl), which bill to the
// same session.
func SessionFiles(transcriptPath string) []string {
	if transcriptPath == "" {
		return nil
	}
	files := []string{transcriptPath}
	sub, _ := filepathGlob(strings.TrimSuffix(transcriptPath, ".jsonl") + "/subagents/agent-*.jsonl")
	return append(files, sub...)
}

// SessionUsage totals one session (subagents included) and, separately,
// what its subagents used.
type SessionUsage struct {
	File
	Subagents Totals `json:"subagents"`
}

// ForSession scans a session's transcript and its subagents.
func (s *Scanner) ForSession(transcriptPath string) SessionUsage {
	var out SessionUsage
	for i, p := range SessionFiles(transcriptPath) {
		f, err := s.Scan(p)
		if err != nil {
			continue
		}
		Merge(&out.File, f)
		if i > 0 {
			out.Subagents.add(f.Total)
		}
	}
	return out
}

// SessionCost is one row of a summary's "top sessions".
type SessionCost struct {
	SessionID string  `json:"session_id"`
	Cost      float64 `json:"cost"`
	Tokens    int64   `json:"tokens"`
}

// Summary is usage across sessions for the last Days days.
type Summary struct {
	Days  int               `json:"days"`
	ByDay map[string]Totals `json:"by_day"`
	Today Totals            `json:"today"`
	Range Totals            `json:"range"`
	Top   []SessionCost     `json:"top"`
}

// Summarize totals the sessions active in the last days days (by their
// transcript's mtime), counting only usage dated inside the window.
func (s *Scanner) Summarize(transcripts map[string]string, days int) Summary {
	if days <= 0 {
		days = 7
	}
	now := time.Now()
	from := now.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	today := now.Format("2006-01-02")
	out := Summary{Days: days, ByDay: map[string]Totals{}}
	cutoff := now.AddDate(0, 0, -days-1)
	for sid, path := range transcripts {
		if fi, err := os.Stat(path); err != nil || fi.ModTime().Before(cutoff) {
			continue
		}
		u := s.ForSession(path)
		var mine Totals
		for day, t := range u.ByDay {
			if day < from {
				continue
			}
			d := out.ByDay[day]
			d.add(t)
			out.ByDay[day] = d
			out.Range.add(t)
			mine.add(t)
			if day == today {
				out.Today.add(t)
			}
		}
		if mine.Messages > 0 {
			out.Top = append(out.Top, SessionCost{SessionID: sid, Cost: mine.Cost, Tokens: mine.Tokens()})
		}
	}
	sortTop(out.Top)
	if len(out.Top) > 10 {
		out.Top = out.Top[:10]
	}
	return out
}
