package id

import (
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"
)

var idFormat = regexp.MustCompile(`^[a-z]+_[a-z0-9]+_\d{10}_[0-9a-f]{6}$`)

func TestNew_Format(t *testing.T) {
	cases := []struct {
		prefix, agent string
	}{
		{PrefixMemory, "ag01"},
		{PrefixHypothesis, "ag01"},
		{PrefixNote, "abcd"},
		{PrefixReview, "ag02"},
		{PrefixEvent, "ag01"},
		{PrefixAgent, "ag01"},
	}
	for _, c := range cases {
		got := New(c.prefix, c.agent)
		if !idFormat.MatchString(got) {
			t.Errorf("New(%q,%q) = %q; does not match format regex", c.prefix, c.agent, got)
		}
		p, err := Parse(got)
		if err != nil {
			t.Fatalf("Parse(%q): %v", got, err)
		}
		if p.Prefix != c.prefix || p.AgentShort != c.agent {
			t.Errorf("Parse(%q) = %+v; want prefix=%s agent=%s", got, p, c.prefix, c.agent)
		}
	}
}

func TestNew_SortableAcrossTimestamps(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	var ids []string
	for i := 0; i < 10; i++ {
		ids = append(ids, newAt(PrefixMemory, "ag01", base.Add(time.Duration(i)*time.Second)))
	}
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("string sort != insertion order; got %v want %v", sorted, ids)
		}
	}
}

func TestNew_UniqueConcurrent(t *testing.T) {
	const total = 1000
	ids := make(chan string, total)
	var wg sync.WaitGroup
	wg.Add(total)
	for i := 0; i < total; i++ {
		go func() {
			defer wg.Done()
			ids <- New(PrefixMemory, "ag01")
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]struct{}, total)
	for id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate ID: %s", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != total {
		t.Fatalf("expected %d unique ids, got %d", total, len(seen))
	}
}

func TestParse_Invalid(t *testing.T) {
	bad := []string{
		"",
		"mem",
		"mem_ag01",
		"mem_ag01_1713345600",
		"mem_ag01_notanumber_abcdef",
		"___abcdef",
	}
	for _, in := range bad {
		if _, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) returned nil error; want ErrInvalid", in)
		}
	}
}
