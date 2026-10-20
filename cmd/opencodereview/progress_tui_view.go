// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/alibaba/open-code-review/internal/progress"
)

// The dashboard is five stacked regions:
//
//	header   what is being reviewed, on what, for how long
//	bar      overall file progress plus the counters worth watching
//	work     one row per unit of concurrent work, live
//	log      the scrolling activity feed
//	help     the keybindings
//
// Everything above the log repaints in place; the log is the only region with
// history. The work list and the log split whatever height the fixed rows
// leave, so the frame always fits the terminal exactly.

const (
	// fixedRows are the regions whose height does not vary: header, bar,
	// stats, a blank separator, and the help line.
	fixedRows = 5

	// minWorkRows and minLogRows are the floors for the two flexible regions.
	// Below these a run would show headers and nothing else, which is worse
	// than showing less of everything.
	minWorkRows = 2
	minLogRows  = 3

	// maxWorkRows stops a run with fifty in-flight groups from crowding the
	// log off the screen entirely.
	maxWorkRows = 12

	// minFrameWidth and minFrameHeight are the floor for the reported window.
	// Every region clips itself to the frame width, so a zero would blank the
	// dashboard instead of just looking cramped. A terminal below the floor
	// gets the too-small message rather than a frame that does not fit.
	minFrameWidth  = 40
	minFrameHeight = 12
)

// tooSmallMessage is what a terminal too small for the dashboard is told
// instead of being shown a frame that would not fit. It is derived from the
// floor so it cannot drift from it.
var tooSmallMessage = fmt.Sprintf(
	"Terminal too small for the dashboard — enlarge it to at least %dx%d, or press Ctrl+C to stop.",
	minFrameWidth, minFrameHeight)

// The dashboard shares the palette provider_tui.go declares for the config
// wizards, so the two terminal interfaces look like one program rather than
// two. The styles the provider flow does not have are declared here, in the
// same tui*Style shape.
var (
	tuiOKStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	tuiWarnStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	tuiBusyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	tuiSectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("8"))
	// The bar's segment colours. Reused is dim because it is inherited
	// rather than earned by this run; done is green; failures are red, so
	// they are visible in the bar itself and not only in the status row.
	tuiReusedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	tuiDoneStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
)

// paint renders s with style, or plainly when color is off.
func (m dashboardModel) paint(style lipgloss.Style, s string) string {
	if !m.color {
		return tuiItemStyle.Render(s)
	}
	return style.Render(s)
}

// View draws the whole dashboard. The frame is composed with lipgloss joins
// rather than a viewport so the top regions stay pinned while the log scrolls.
func (m dashboardModel) View() tea.View {
	if m.tooSmall() {
		return tea.NewView(m.paint(tuiDimStyle, tooSmallMessage))
	}
	if m.detached {
		// Update already quits on detach, so a frame rendered on the way out
		// must not paint over the text the runner has just restored.
		return tea.NewView("")
	}

	regions := []string{
		m.viewHeader(),
		m.viewBar(),
		"",
		m.viewWork(),
		m.viewLog(),
		m.viewHelp(),
	}
	// Every region is already clipped to the frame width and none sets a
	// background, so a plain join is equivalent to lipgloss's and skips its
	// per-line re-measurement on every frame.
	v := tea.NewView(strings.Join(regions, "\n"))
	v.AltScreen = true
	return v
}

// tooSmall reports whether the terminal is too narrow or too short for the
// dashboard to draw itself.
//
// A reported zero is the pty that has not been sized yet, not a terminal
// that cannot be shown, so it does not count.
func (m dashboardModel) tooSmall() bool {
	return (m.rawWidth > 0 && m.rawWidth < minFrameWidth) ||
		(m.rawHeight > 0 && m.rawHeight < minFrameHeight)
}

func (m dashboardModel) viewHeader() string {
	left := m.paint(tuiTitleStyle, "ocr "+m.title)
	if m.repo != "" {
		left += "  " + m.paint(tuiDimStyle, shortenPath(m.repo, m.width/3))
	}
	if m.subtitle != "" {
		left += "  " + m.paint(tuiDimStyle, m.subtitle)
	}
	right := m.paint(tuiDimStyle, humanDuration(int64(time.Since(m.startedAt).Seconds())))
	ident := identity(m.provider, m.modelName)
	if ident == "" {
		return joinEnds(m.width, left, right)
	}
	return joinEnds(m.width, left, m.paint(tuiTitleStyle, ident)+m.tokenBadge()+"  "+right)
}

// tokenBadge is the run's cumulative spend, beside the model name: arrows
// rather than words, because the two numbers are read constantly and the
// labels cost more width than they are worth.
func (m dashboardModel) tokenBadge() string {
	if m.inputTokens == 0 && m.outputTokens == 0 {
		return ""
	}
	in := m.paint(tuiBusyStyle, "▲"+humanTokens(m.inputTokens))
	out := m.paint(tuiWarnStyle, "▼"+humanTokens(m.outputTokens))
	return m.paint(tuiDimStyle, " [") + in + m.paint(tuiDimStyle, " ") + out + m.paint(tuiDimStyle, "]")
}

func (m dashboardModel) viewBar() string {
	row := lipgloss.JoinHorizontal(lipgloss.Top,
		m.paint(tuiDimStyle, "reviewing"),
		" ",
		m.viewBarFill(),
		" ",
		m.paint(tuiTitleStyle, fmt.Sprintf("%d/%d", m.finishedFiles(), m.totalFiles)),
		" ",
		m.viewBarLegend(),
	)
	return lipgloss.JoinVertical(lipgloss.Left, clip(row, m.width), m.viewStats())
}

// viewBarFill draws the progress bar itself.
//
// This is hand-rolled rather than using bubbles' progress component, which
// animates a spring toward its target. Files complete in discrete steps at
// LLM-round granularity, so a spring would spend frames animating between
// values a user reads as the same number, and it would pull in a physics
// dependency for the privilege.
func (m dashboardModel) viewBarFill() string {
	cells := m.barCells()
	body := ""
	if m.totalFiles > 0 {
		// The bar is segmented rather than a single fill, because "already
		// reviewed in a previous run" and "reviewed during this one" are very
		// different states for the reader: the first means the run inherited
		// its result, the second means it is doing the work. Reusing a
		// monochrome fill made a resumed run look further along than the
		// effort actually being spent.
		part := func(n int) int {
			c := int(float64(n) / float64(m.totalFiles) * float64(cells))
			switch {
			case c < 0:
				return 0
			case c > cells:
				return cells
			default:
				return c
			}
		}
		reused := part(m.reusedFiles)
		done := part(m.doneFiles)
		failed := part(m.failedFiles)

		body = m.paint(tuiReusedStyle, strings.Repeat("█", reused)) +
			m.paint(tuiDoneStyle, strings.Repeat("█", done)) +
			m.paint(tuiErrorStyle, strings.Repeat("█", failed))
		if used := reused + done + failed; used < cells {
			body += m.paint(tuiDimStyle, strings.Repeat("░", cells-used))
		}
	} else {
		body = m.paint(tuiDimStyle, strings.Repeat("░", cells))
	}
	return m.paint(tuiDimStyle, "▕") + body + m.paint(tuiDimStyle, "▏")
}

// viewBarLegend names the bar's segments. A bar in three colours is not
// self-explanatory, and the reused segment in particular would otherwise be
// guesswork: only the count beside it says what the first block means.
func (m dashboardModel) viewBarLegend() string {
	seg := func(style lipgloss.Style, label string, n int) string {
		if n == 0 {
			return ""
		}
		return m.paint(style, "▇") + m.paint(tuiDimStyle, " "+label)
	}
	parts := []string{
		seg(tuiReusedStyle, "reused", m.reusedFiles),
		seg(tuiDoneStyle, "done", m.doneFiles),
		seg(tuiErrorStyle, "failed", m.failedFiles),
	}
	out := ""
	for i, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += m.paint(tuiDimStyle, " ")
		}
		out += p
		_ = i
	}
	return out
}

// barCells is how wide the bar may be. It is derived from the terminal rather
// than fixed, because a fixed bar overflows a narrow window and pushes every
// region below it off screen. The overhead is "reviewing ", the two frame
// glyphs, and the "n/m" counter.
func (m dashboardModel) barCells() int {
	const overhead = len("reviewing  ▕▏ ") + 7
	cells := m.width - overhead
	switch {
	case cells > 24:
		return 24
	case cells < 4:
		return 4
	default:
		return cells
	}
}

// viewStats is the counter row: everything a person would otherwise have to
// grep the text wall for, in one line.
func (m dashboardModel) viewStats() string {
	parts := []string{
		m.paint(tuiOKStyle, fmt.Sprintf("%d findings", m.findings)),
		m.paint(tuiDimStyle, "·"),
		m.paint(tuiBusyStyle, fmt.Sprintf("%d in flight", m.countRunning())),
	}
	if m.reusedFiles > 0 {
		parts = append(parts, m.paint(tuiDimStyle, "·"), m.paint(tuiReusedStyle, fmt.Sprintf("%d reused", m.reusedFiles)))
	}
	if m.failedFiles > 0 {
		parts = append(parts, m.paint(tuiDimStyle, "·"), m.paint(tuiErrorStyle, fmt.Sprintf("%d failed", m.failedFiles)))
	}
	if m.skippedFiles > 0 {
		parts = append(parts, m.paint(tuiDimStyle, "·"), m.paint(tuiWarnStyle, fmt.Sprintf("%d skipped", m.skippedFiles)))
	}
	// The one thing that must never be buried: a run that is about to stop
	// early has to say so in the status line, not only in the log.
	if m.budgetHit {
		parts = append(parts, m.paint(tuiDimStyle, "·"), m.paint(tuiWarnStyle, "token budget reached — stopping early"))
	}
	if m.paused {
		parts = append(parts, m.paint(tuiDimStyle, "·"), m.paint(tuiWarnStyle, "log paused"))
	}
	row := lipgloss.JoinHorizontal(lipgloss.Top, parts...)

	// The session id goes last so it can never displace a counter, and it is
	// trimmed from the left if the terminal is too narrow: the tail is the
	// part that identifies a session, and it is what a reader compares
	// against the dashboard.
	if m.sessionID != "" {
		id := m.sessionID
		budget := m.width - lipgloss.Width(row) - len("· session ")
		if budget > 8 {
			if len(id) > budget {
				id = "…" + id[len(id)-budget+1:]
			}
			row += m.paint(tuiDimStyle, "· session ") + m.paint(tuiDimStyle, id)
		}
	}
	return clip(row, m.width)
}

// viewWork renders the concurrent work list. Running rows sort first because
// they are the answer to "what is it doing right now"; finished rows sink
// below them, most recent first.
func (m dashboardModel) viewWork() string {
	running, done := m.splitItems()
	rows := make([]string, 0, len(running)+len(done)+2)

	frame := m.spinner.View()
	for _, it := range running {
		rows = append(rows, m.viewItem(it, frame))
	}
	if len(running) == 0 {
		rows = append(rows, m.paint(tuiDimStyle, "  waiting for work…"))
	}

	// The running list scrolls independently: a wide review can have more
	// groups in flight than the region has rows.
	if len(running) > m.workBodyRows() {
		rows = m.scrollWindow(rows, m.workTop)
	}

	if m.showCompleted {
		rows = append(rows, m.sectionHeading("completed", len(done), ""))
		for _, it := range done {
			rows = append(rows, m.viewItem(it, frame))
		}
	}

	hint := fmt.Sprintf("%d running", m.countRunning())
	if len(done) > 0 && !m.showCompleted {
		hint = fmt.Sprintf("%d running · %d done (c)", m.countRunning(), len(done))
	}
	heading := m.sectionHeading("work", len(m.items), hint)
	if m.focus == dashboardPaneWork {
		// Mark the pane the arrows are driving, so a key press is never a
		// guess about which list just moved.
		heading = m.paint(tuiTitleStyle, heading)
	}
	return m.clamp(heading, rows, m.workHeight())
}

// splitItems partitions the work list. Running rows come first because they
// are the answer to "what is it doing"; finished rows keep their most-recent
// -first order so the most recent completion is the one you see.
func (m dashboardModel) splitItems() (running, done []*dashboardItem) {
	for _, it := range m.items {
		if it.state == dashboardStateRunning {
			running = append(running, it)
		}
	}
	for i := len(m.items) - 1; i >= 0; i-- {
		if m.items[i].state != dashboardStateRunning {
			done = append(done, m.items[i])
		}
	}
	return running, done
}

// workRows is how many rows the work region can show, heading included.
func (m dashboardModel) workRows() int {
	return m.workHeight()
}

// workBodyRows is the room left for rows once the heading is drawn. When the
// completed section is expanded it takes the rest, so the running list keeps
// its own room rather than being squeezed away by history.
func (m dashboardModel) workBodyRows() int {
	rows := m.workHeight() - 1
	if m.showCompleted {
		rows /= 2
	}
	if rows < 1 {
		rows = 1
	}
	return rows
}

// scrollWindow shows a window of rows at offset, marking that more exist in
// each direction so a partially visible list is never mistaken for a whole
// one.
func (m dashboardModel) scrollWindow(rows []string, top int) []string {
	height := m.workBodyRows()
	if len(rows) <= height {
		return rows
	}
	if top > len(rows)-height {
		top = len(rows) - height
	}
	if top < 0 {
		top = 0
	}
	window := rows[top : top+height]
	if top > 0 {
		window = append([]string{m.paint(tuiDimStyle, fmt.Sprintf("  ↑ %d more", top))}, window...)
		window = window[:height]
	}
	if top+height < len(rows) {
		window[len(window)-1] += m.paint(tuiDimStyle, fmt.Sprintf(" ↓%d more", len(rows)-top-height))
	}
	return window
}

func (m dashboardModel) viewItem(it *dashboardItem, frame string) string {
	var icon, name, note string
	switch it.state {
	case dashboardStateRunning:
		icon = frame
		name = m.paint(tuiTitleStyle, it.display())
		note = m.itemNote(it)
	case dashboardStateDone:
		icon = m.paint(tuiOKStyle, "✓")
		name = m.paint(tuiDimStyle, it.display())
		note = m.paint(tuiDimStyle, m.costLine(it))
	case dashboardStateFailed:
		icon = m.paint(tuiErrorStyle, "✗")
		name = m.paint(tuiErrorStyle, it.display())
		note = m.paint(tuiErrorStyle, it.err)
	}
	// A fixed icon column and a fixed name column are what make a dozen
	// simultaneous rows scannable rather than a ragged mess.
	row := "  " + pad(icon, 2) + " " + pad(name, m.nameWidth()) + " " + note
	return clip(row, m.width)
}

// costLine is the right-hand column of a finished row: what it found, how long
// it took, and what it spent. Findings first because that is why anyone looks,
// then the two numbers that tell the story of the cost.
func (m dashboardModel) costLine(it *dashboardItem) string {
	return fmt.Sprintf("%d findings · %s · %s",
		it.findings, shortDuration(it.took), humanTokens(it.tokens))
}

// itemNote is the right-hand column of a running row: which round, what the
// agent is doing right now, and what it has spent so far.
func (m dashboardModel) itemNote(it *dashboardItem) string {
	var b strings.Builder
	// Rounds are published starting at 1, so a non-zero round is what
	// distinguishes "in a round" from "still starting".
	if it.round > 0 {
		b.WriteString(m.paint(tuiDimStyle, fmt.Sprintf("round %d", it.round)))
	}
	if it.tool != "" {
		if b.Len() > 0 {
			b.WriteString(m.paint(tuiDimStyle, " · "))
		}
		b.WriteString(m.paint(tuiBusyStyle, it.tool))
	}
	if b.Len() == 0 {
		b.WriteString(m.paint(tuiDimStyle, "starting…"))
	}
	// Spend so far, kept last so the round and the current tool — the parts
	// that change as the work proceeds — stay on the left where the eye lands.
	if it.tokens > 0 {
		if b.Len() > 0 {
			b.WriteString(m.paint(tuiDimStyle, " · "))
		}
		b.WriteString(m.paint(tuiBusyStyle, "▲"+humanTokens(it.tokens)))
	}
	return b.String()
}

// viewLog is the scrolling activity feed. It is the only region with history,
// so it takes whatever height the work list does not need.
func (m dashboardModel) viewLog() string {
	heading := m.sectionHeading("activity", len(m.log), logHint(m.paused, m.holding()))

	height := m.logHeight()
	// One line is spent on the heading, so the body gets the rest.
	avail := height - 1
	if avail < 1 {
		avail = 1
	}
	// The window ends where the reader left it: logOffset lines back from the
	// tail, minus however many lines arrived while paused. Both offsets count
	// backwards from the end, so subtracting them pins the window across a
	// pause without having to freeze the log itself.
	end := len(m.log) - m.logOffset - m.pending
	if end < 0 {
		end = 0
	}
	start := end - avail
	if start < 0 {
		start = 0
	}
	rows := make([]string, 0, end-start)
	for _, e := range m.log[start:end] {
		rows = append(rows, m.viewLogEntry(e))
	}
	if len(rows) == 0 {
		rows = append(rows, m.paint(tuiDimStyle, "  no activity yet"))
	}
	return m.clamp(heading, rows, height)
}

func (m dashboardModel) viewLogEntry(e dashboardLogEntry) string {
	stamp := m.paint(tuiDimStyle, e.stamp)
	var body string
	switch e.kind {
	case progress.KindWarning, progress.KindGroupFailed, progress.KindToolError, progress.KindBudgetReached:
		body = m.paint(tuiWarnStyle, e.text)
	case progress.KindGroupStart, progress.KindGroupDone, progress.KindRoundStart, progress.KindSkipped:
		body = m.paint(tuiDimStyle, e.text)
	default:
		body = e.text
	}
	return clip("  "+stamp+"  "+body, m.width)
}

// helpKeys names the scroll hint after the focused pane: one pair of arrows
// now drives two lists, so the binding alone is not enough to know which one
// will move.
func (m dashboardModel) helpKeys() []string {
	return []string{
		"q detach",
		"tab pane",
		"↑↓ " + m.focusName(),
		"c completed",
		"p pause log",
		"ctrl+c cancel",
	}
}

func (m dashboardModel) focusName() string {
	if m.focus == dashboardPaneWork {
		return "work"
	}
	return "log"
}

func (m dashboardModel) viewHelp() string {
	sep := m.paint(tuiDimStyle, "  ·  ")
	help := m.paint(tuiDimStyle, strings.Join(m.helpKeys(), sep))
	return clip(help, m.width)
}

// sectionHeading is the small label above a region, with a right-aligned hint.
func (m dashboardModel) sectionHeading(name string, count int, hint string) string {
	left := m.paint(tuiSectionStyle, name)
	if count > 0 {
		left += m.paint(tuiDimStyle, fmt.Sprintf(" (%d)", count))
	}
	if hint == "" {
		return left
	}
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(hint)
	if gap < 2 {
		return left
	}
	return left + strings.Repeat(" ", gap) + m.paint(tuiDimStyle, hint)
}

// clamp joins a heading to its rows and cuts the result to exactly height
// lines, so a long list can never push the keybindings off screen.
func (m dashboardModel) clamp(heading string, rows []string, height int) string {
	all := append([]string{heading}, rows...)
	if height > 0 && len(all) > height {
		all = all[:height]
	}
	return strings.Join(all, "\n")
}

// Layout arithmetic: the work list and the log split whatever the fixed rows
// leave, each with a floor and the work list with a ceiling.

func (m dashboardModel) flexibleRows() int {
	rows := m.height - fixedRows
	if rows < minWorkRows+minLogRows {
		return minWorkRows + minLogRows
	}
	return rows
}

func (m dashboardModel) workHeight() int {
	rows := m.flexibleRows() / 3
	if rows < minWorkRows {
		rows = minWorkRows
	}
	if rows > maxWorkRows {
		rows = maxWorkRows
	}
	return rows + 1 // heading
}

// logHeight is whatever the work list did not take. workHeight counts its own
// heading, so the subtraction below is against the body rows alone.
func (m dashboardModel) logHeight() int {
	return m.flexibleRows() - (m.workHeight() - 1)
}

func (m dashboardModel) countRunning() int { return m.running }

// finishedFiles is the bar's numerator: files the run is actually done with.
//
// Skipped files are deliberately excluded. The denominator is the count of
// files selected for review, and selection has already removed everything that
// was skipped — so counting them again here would measure progress against a
// total that never contained them. A scan that discovered 759 files, skipped
// 272 and selected 487 would open at 320/487 instead of 0/487. They are not
// lost: the status row reports the skipped count separately.
func (m dashboardModel) finishedFiles() int {
	return m.reusedFiles + m.doneFiles + m.failedFiles
}

func (m dashboardModel) nameWidth() int {
	w := m.width - 12
	switch {
	case w < 16:
		return 16
	case w > 60:
		return 60
	default:
		return w
	}
}

func logHint(paused, scrub bool) string {
	switch {
	case paused:
		return "paused — press p to resume"
	case scrub:
		return "held — press u to follow"
	default:
		return "following"
	}
}

// joinEnds puts left and right on one line with the gap between them, or
// drops right when the terminal is too narrow to hold both.
func joinEnds(width int, left, right string) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 2 {
		return clip(left, width)
	}
	return left + strings.Repeat(" ", gap) + right
}

func identity(provider, dashboardModel string) string {
	switch {
	case provider != "" && dashboardModel != "":
		return provider + "/" + dashboardModel
	case dashboardModel != "":
		return dashboardModel
	default:
		return provider
	}
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "<1s"
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// pad right-pads to n visible cells, counting ANSI escapes as zero width.
func pad(s string, n int) string {
	gap := n - lipgloss.Width(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}

// clip hard-wraps to n cells. A hard cut rather than an ellipsis, because
// these rows are status lines that update many times a second: a trailing
// ellipsis on every row would be noise, and a wrapping status line would
// break the fixed height the layout depends on.
// clip cuts s to n columns, marking the cut with an ellipsis.
//
// ansi.Truncate counts the printable columns of a styled string rather than
// its bytes, so a row that carries colour is cut where it looks cut. The
// ellipsis says the row continues past the edge, which a hard cut does not.
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

// shortenPath keeps the tail of a path, which is the part that identifies it.
func shortenPath(p string, width int) string {
	if width <= 0 {
		return p
	}
	runes := []rune(p)
	if len(runes) <= width {
		return p
	}
	if width <= 1 {
		return string(runes[len(runes)-width:])
	}
	return "…" + string(runes[len(runes)-(width-1):])
}
