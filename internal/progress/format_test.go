// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package progress

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"testing"
	"time"
)

func TestHumanTokens(t *testing.T) {
	// The abbreviation ladder, including both boundaries where the format
	// changes: a leading zero must not survive as "1.k", and two-digit
	// mantissas must not carry a decimal.
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{-5, "0"},
		{7, "7"},
		{999, "999"},
		{1_000, "1k"},
		{1_050, "1.1k"},
		{3_800, "3.8k"},
		{9_949, "9.9k"},
		{10_000, "10k"},
		{12_400, "12k"},
		{123_000, "123k"},
		{999_999, "999k"},
		{1_000_000, "1M"},
		{1_050_000, "1.1M"},
		{12_400_000, "12M"},
	}
	for _, c := range cases {
		if got := humanTokens(c.in); got != c.want {
			t.Errorf("humanTokens(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanTokensNeverOverflowsItsColumn(t *testing.T) {
	// The point of abbreviating is that the widest value fits a narrow frame.
	for _, n := range []int64{0, 999, 1_000, 3_800, 99_999, 1_000_000, 999_000_000} {
		if got := len(humanTokens(n)); got > 6 {
			t.Errorf("humanTokens(%d) = %q is %d chars, too wide for a row column", n, humanTokens(n), got)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{-1, "0s"},
		{0, "0s"},
		{9, "9s"},
		{59, "59s"},
		{60, "1m00s"},
		{3661, "1h01m"},
	}
	for _, c := range cases {
		if got := humanDuration(c.in); got != c.want {
			t.Errorf("humanDuration(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTokenBadgeHiddenUntilThereIsUsage(t *testing.T) {
	m := testModel(80, 24)
	if got := m.tokenBadge(); got != "" {
		t.Errorf("no usage yet must render no badge, got %q", got)
	}
	m.inputTokens, m.outputTokens = 3_800, 1_200
	badge := stripANSI(m.tokenBadge())
	if !strings.Contains(badge, "▲3.8k") || !strings.Contains(badge, "▼1.2k") {
		t.Errorf("badge = %q, want input and output marked by arrow", badge)
	}
}

func TestCostLineCarriesFindingsTimeAndSpend(t *testing.T) {
	it := &item{findings: 7, took: 5 * time.Minute, tokens: 214_000}
	got := stripANSI(newModel(Options{}, 0).costLine(it))
	for _, want := range []string{"7 findings", "5m00s", "214k"} {
		if !strings.Contains(got, want) {
			t.Errorf("cost line %q is missing %q", got, want)
		}
	}
}

func TestRunningRowKeepsTokensBeforeAnyRoundOrTool(t *testing.T) {
	// A row can spend tokens before it has a round number or a tool name —
	// the plan phase does exactly that. The spend must still show.
	m := testModel(100, 30)
	m = feed(m,
		Event{Kind: KindGroupStart, Group: "a.go", Paths: []string{"a.go"}},
		Event{Kind: KindUsage, Group: "a.go", InputTokens: 1000, OutputTokens: 1000},
	)
	note := stripANSI(m.itemNote(m.lookup("a.go")))
	if !strings.Contains(note, "▲2k") {
		t.Errorf("a starting row must still show its spend, got %q", note)
	}
}

func TestHelpNamesTheFocusedPane(t *testing.T) {
	m := testModel(80, 24)
	if !strings.Contains(m.paint(styleDim, strings.Join(m.helpKeys(), " ")), "work") {
		t.Error("the scroll hint must name the work pane when it is focused")
	}
	m.focus = paneActivity
	if !strings.Contains(m.paint(styleDim, strings.Join(m.helpKeys(), " ")), "log") {
		t.Error("the scroll hint must name the log when it is focused")
	}
}

func TestScrollWindowMarksBothEnds(t *testing.T) {
	m := testModel(80, 30)
	rows := make([]string, 30)
	for i := range rows {
		rows[i] = "row"
	}
	// At the top there is nothing above, so only the downward marker appears.
	top := stripANSI(strings.Join(m.scrollWindow(rows, 0), "\n"))
	if strings.Contains(top, "↑") {
		t.Errorf("a window at the top has nothing above it, got %q", top)
	}
	if !strings.Contains(top, "↓") {
		t.Errorf("a window at the top must say there is more below, got %q", top)
	}
	mid := stripANSI(strings.Join(m.scrollWindow(rows, 10), "\n"))
	if !strings.Contains(mid, "↑") || !strings.Contains(mid, "↓") {
		t.Errorf("a window in the middle must mark both ends, got %q", mid)
	}
	// An offset past the end clamps rather than panicking or going blank.
	if got := m.scrollWindow(rows, 999); len(got) == 0 {
		t.Error("an out-of-range offset must still render something")
	}
}

func TestWorkBodyRowsHalvesForTheCompletedSection(t *testing.T) {
	m := testModel(80, 30)
	full := m.workBodyRows()
	m.showCompleted = true
	if half := m.workBodyRows(); half >= full {
		t.Errorf("expanding completed must leave the running list less room: %d vs %d", half, full)
	}
	if m.workBodyRows() < 1 {
		t.Error("the running list must always keep at least one row")
	}
}

func TestOppositePane(t *testing.T) {
	if oppositePane(paneWork) != paneActivity || oppositePane(paneActivity) != paneWork {
		t.Error("tab must toggle between the two panes")
	}
}

func TestTickPollsTokenTotals(t *testing.T) {
	// The run-wide totals are polled on the render tick, not pushed, so the
	// tick is what has to pick them up.
	m := newModel(Options{
		Title:  "review",
		Tokens: func() (int64, int64) { return 4200, 900 },
	}, 1)
	upd, _ := m.Update(tickMsg(time.Now()))
	m = upd.(model)
	if m.inputTokens != 4200 || m.outputTokens != 900 {
		t.Errorf("tick must poll the totals, got in=%d out=%d", m.inputTokens, m.outputTokens)
	}
}

func TestPublishIsSafeForConcurrentGroups(t *testing.T) {
	// The pipeline publishes from one goroutine per in-flight group.
	var buf = new(strings.Builder)
	restore := captureStdoutFor(buf)
	defer restore()

	install(t, &textSink{})
	done := make(chan struct{})
	for i := range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 20 {
				Noticef("group %d line\n", i)
			}
		}()
	}
	for range 8 {
		<-done
	}
	if got := strings.Count(buf.String(), "\n"); got != 160 {
		t.Errorf("expected 160 lines, got %d — a write was torn", got)
	}
}

func TestSessionIDAppearsWhenKnown(t *testing.T) {
	// The id is empty until the session's first write persists, so it arrives
	// after the dashboard is already up. It must appear without a redraw
	// request from the caller.
	id := ""
	m := newModel(Options{
		Title:   "scan",
		Session: func() string { return id },
	}, 100)
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 110, Height: 24})
	m = upd.(model)

	if got := stripANSI(m.viewStats()); strings.Contains(got, "session") {
		t.Errorf("no session id yet, but the row shows one: %q", got)
	}

	id = "3ee6cd77-99bb-48d4-af7b-6eaa3ccbdcb7"
	upd, _ = m.Update(tickMsg(time.Now()))
	m = upd.(model)
	if m.sessionID != id {
		t.Fatalf("sessionID = %q, want %q", m.sessionID, id)
	}
	stats := stripANSI(m.viewStats())
	if !strings.Contains(stats, id) {
		t.Errorf("the row must show the id for cross-checking, got %q", stats)
	}
}

func TestSessionIDIsTrimmedNotDroppedOnNarrowTerminals(t *testing.T) {
	id := "3ee6cd77-99bb-48d4-af7b-6eaa3ccbdcb7"
	m := newModel(Options{
		Title:   "scan",
		Session: func() string { return id },
	}, 100)
	upd, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 24})
	m = upd.(model)
	m.sessionID = id
	row := stripANSI(m.viewStats())
	if lipgloss.Width(row) > 60 {
		t.Errorf("the row is %d cells wide, wider than the terminal", lipgloss.Width(row))
	}
	// Whatever survives must be the tail: that is the identifying end.
	if strings.Contains(row, "session") && !strings.Contains(row, id[len(id)-8:]) {
		t.Errorf("a trimmed id must keep its tail, got %q", row)
	}
}

func TestSkipsCollapseInTheLogButNotTheCounter(t *testing.T) {
	// Selection skips hundreds of files for a handful of reasons. One log row
	// per reason keeps the feed readable; the counter still sees every file.
	m := testModel(110, 30)
	for i := range 270 {
		m = feed(m, Event{Kind: KindSkipped, Detail: fmt.Sprintf("f%03d.ts", i),
			SkipReason: "filtered by path/extension rules"})
	}
	for range 2 {
		m = feed(m, Event{Kind: KindSkipped, Detail: "big.html",
			SkipReason: "exceeds 80% of max_tokens"})
	}
	m = feed(m, Event{Kind: KindNotice, Text: "full-scan: 759 file(s) discovered, reviewing 487\n"})

	if m.skippedFiles != 272 {
		t.Errorf("skippedFiles = %d, want 272", m.skippedFiles)
	}
	if len(m.log) != 3 {
		t.Fatalf("expected 2 collapsed rows plus the notice, got %d: %+v", len(m.log), m.log)
	}
	got := stripANSI(m.viewLog())
	if !strings.Contains(got, "Skipping 270 file(s) — filtered by path/extension rules") {
		t.Errorf("the collapsed row must name its count and reason:\n%s", got)
	}
	if !strings.Contains(got, "exceeds 80% of max_tokens") {
		t.Errorf("a different reason must get its own row:\n%s", got)
	}
}

func TestSkippedFilesPrintNoTextLine(t *testing.T) {
	// The caller prints its own per-file line; the skip event must not add a
	// second one to piped output.
	var buf = new(strings.Builder)
	restore := captureStdoutFor(buf)
	defer restore()

	install(t, &textSink{})
	Skipped("a.ts", "binary file", "Skipping a.ts — binary file\n")
	if got := buf.String(); strings.Count(got, "Skipping") != 1 {
		t.Errorf("expected exactly one line, got %q", got)
	}
}

// TestSkipsPrintOnceAndCollapse is the regression guard for a doubling that
// was visible on a real scan: every skipped file produced two identical rows
// in the activity log, because the pipeline emitted a notice line and a skip
// event for the same file.
func TestSkipsPrintOnceAndCollapse(t *testing.T) {
	var out = new(strings.Builder)
	restore := captureStdoutFor(out)
	defer restore()

	install(t, &textSink{})
	m := testModel(110, 30)
	for i := range 270 {
		p := fmt.Sprintf("tests/t%03d.test.ts", i)
		Skipped(p, "filtered by path/extension rules",
			fmt.Sprintf("Skipping %s — filtered by path/extension rules\n", p))
		m = feed(m, Event{Kind: KindSkipped, Detail: p,
			SkipReason: "filtered by path/extension rules",
			Text:       fmt.Sprintf("Skipping %s — filtered by path/extension rules\n", p)})
	}

	// Piped output keeps every per-file line: it is the record of what was
	// left out of the run.
	if got := strings.Count(out.String(), "\n"); got != 270 {
		t.Errorf("text output has %d lines, want 270", got)
	}
	// The dashboard shows one collapsed row instead.
	if len(m.log) != 1 {
		t.Fatalf("dashboard log has %d rows, want 1 collapsed row: %+v", len(m.log), m.log)
	}
	got := stripANSI(m.viewLog())
	if strings.Contains(got, "tests/t000.test.ts") {
		t.Errorf("the collapsed row must not list individual files:\n%s", got)
	}
	if !strings.Contains(got, "Skipping 270 file(s) — filtered by path/extension rules") {
		t.Errorf("collapsed row is wrong:\n%s", got)
	}
}
