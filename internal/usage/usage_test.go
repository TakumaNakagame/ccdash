package usage

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestScanDedupesAndIncrements(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	line := func(id, model string, in, out, cw, cr int64) string {
		return `{"type":"assistant","timestamp":"2026-10-05T03:00:00Z","message":{"id":"` + id + `","model":"` + model +
			`","usage":{"input_tokens":` + itoa(in) + `,"output_tokens":` + itoa(out) + `,"cache_creation_input_tokens":` + itoa(cw) +
			`,"cache_read_input_tokens":` + itoa(cr) + `,"cache_creation":{"ephemeral_1h_input_tokens":` + itoa(cw) + `,"ephemeral_5m_input_tokens":0}}}}` + "\n"
	}
	body := line("m1", "claude-opus-5-5", 1000, 2000, 10000, 100000) +
		line("m1", "claude-opus-5-5", 1000, 2000, 10000, 100000) + // streamed duplicate
		`{"type":"user","message":{"content":"hi"}}` + "\n"
	_ = os.WriteFile(p, []byte(body), 0o600)
	s := NewScanner()
	f, err := s.Scan(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.Total.Messages != 1 || f.Total.Output != 2000 || f.Total.CacheRead != 100000 {
		t.Fatalf("totals = %+v", f.Total)
	}
	// opus 5.5: 1000*4 + 2000*20 + 10000*4*2 (1h write) + 100000*0.20 = 4000+40000+80000+20000 = 144000 / 1e6
	if math.Abs(f.Total.Cost-0.144) > 1e-9 {
		t.Fatalf("cost = %v, want 0.144", f.Total.Cost)
	}
	// Append another message (plus a partial line that must wait).
	fh, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = fh.WriteString(line("m2", "claude-haiku-4-5", 1000000, 0, 0, 0) + `{"type":"assistant","message":{"id":"m3"`)
	fh.Close()
	f, _ = s.Scan(p)
	if f.Total.Messages != 2 || math.Abs(f.Total.Cost-(0.144+1.0)) > 1e-9 || len(f.ByModel) != 2 {
		t.Fatalf("after append = %+v %v", f.Total, f.ByModel)
	}
	if _, ok := f.ByDay["2026-10-05"]; !ok {
		t.Errorf("by day = %v", f.ByDay)
	}
	// The newest message names the model and the current context size.
	if f.Model != "claude-haiku-4-5" || f.Context != 1000000 {
		t.Errorf("model/context = %q %d", f.Model, f.Context)
	}
	b := s.Briefs(map[string]string{"sess": p, "none": filepath.Join(t.TempDir(), "missing.jsonl")})
	if len(b) != 1 || b["sess"].Model != "claude-haiku-4-5" || b["sess"].Messages != 2 {
		t.Errorf("briefs = %+v", b)
	}
}

func TestUnknownModelHasNoCost(t *testing.T) {
	if c := cost("gpt-x", rawUsage{Input: 1e6}); c != 0 {
		t.Errorf("cost = %v", c)
	}
	if p, _ := priceOf("claude-opus-4-8"); p.in != 5 {
		t.Errorf("opus 4.8 price = %+v", p)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
