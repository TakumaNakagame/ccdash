package skills

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"
)

// The session's own list: Claude Code writes a "skill_listing" attachment
// into the transcript (the full list at start, deltas when skills are
// added), naming exactly what that session can invoke — bundled skills,
// plugins, synced, user and project ones alike. FromTranscript reads it,
// incrementally per file (a transcript grows to tens of MB).

type listing struct {
	Attachment struct {
		Type    string   `json:"type"`
		Content string   `json:"content"`
		Names   []string `json:"names"`
	} `json:"attachment"`
}

type tsCache struct {
	off   int64
	names []string
	desc  map[string]string
}

var (
	tsMu    sync.Mutex
	tsFiles = map[string]*tsCache{}
)

// maxDesc caps a description: the pickers show one line of it.
const maxDesc = 300

// FromTranscript returns the skills the transcript's skill_listing
// attachments name, in listing order (Source "session"); nil when it has
// none or can't be read.
func FromTranscript(path string) []Skill {
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	tsMu.Lock()
	defer tsMu.Unlock()
	c := tsFiles[path]
	if c == nil || fi.Size() < c.off {
		c = &tsCache{desc: map[string]string{}}
		tsFiles[path] = c
	}
	if fi.Size() > c.off {
		if _, err := f.Seek(c.off, io.SeekStart); err != nil {
			return nil
		}
		rd := bufio.NewReaderSize(f, 256*1024)
		for {
			line, err := rd.ReadBytes('\n')
			if err != nil {
				break // a partial last line is read again next time
			}
			c.off += int64(len(line))
			if !bytes.Contains(line, []byte(`"skill_listing"`)) {
				continue
			}
			var l listing
			if json.Unmarshal(line, &l) != nil || l.Attachment.Type != "skill_listing" {
				continue
			}
			c.add(l.Attachment.Names, l.Attachment.Content)
		}
	}
	out := make([]Skill, 0, len(c.names))
	for _, n := range c.names {
		out = append(out, Skill{Name: n, Description: c.desc[n], Source: "session"})
	}
	return out
}

func (c *tsCache) add(names []string, content string) {
	known := map[string]bool{}
	for _, n := range c.names {
		known[n] = true
	}
	for _, n := range names {
		if validName(n) && !known[n] {
			known[n] = true
			c.names = append(c.names, n)
		}
	}
	// content is "- <name>: <description>" per skill.
	for _, ln := range strings.Split(content, "\n") {
		name, desc, ok := strings.Cut(strings.TrimPrefix(ln, "- "), ": ")
		if !ok || !strings.HasPrefix(ln, "- ") || !known[name] {
			continue
		}
		if r := []rune(desc); len(r) > maxDesc {
			desc = string(r[:maxDesc]) + "…"
		}
		c.desc[name] = strings.TrimSpace(desc)
	}
}
