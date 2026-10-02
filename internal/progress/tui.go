// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package progress

import (
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
)

const (
	// logRetained is how many log entries the model keeps. The log is a
	// diagnostic surface, not a transcript: a long run produces thousands of
	// tool events, and holding them all would grow without bound for no gain.
	logRetained = 500

	// tickInterval drives the spinner and the elapsed clock. It matches the
	// refresh rate closely enough that the two never look out of step.
	tickInterval = 100 * time.Millisecond
)

// itemState is where a unit of concurrent work has got to.
type itemState int

const (
	stateRunning itemState = iota
	stateDone
	stateFailed
)

// pane identifies which region the arrow keys act on.
type pane int

const (
	paneWork pane = iota
	paneActivity
)

// item is one row of the work list. A run dispatches groups of semantically
// related files concurrently, so "which file is it on" is really "which
// groups are in flight and what is each one doing" — one row per group answers
// it, with the group's files behind it.
type item struct {
	label    string
	paths    []string
	state    itemState
	round    int
	tool     string
	started  time.Time
	took     time.Duration
	findings int
	err      string
	// tokens is what this unit of work has spent, republished as cumulative
	// totals after every LLM response so the row is live rather than final.
	tokens int64
}

// display is the short form of the row's subject: the first file, which is what
// a person scanning the list is looking for.
func (it *item) display() string {
	if len(it.paths) == 0 {
		return it.label
	}
	if len(it.paths) == 1 {
		return it.paths[0]
	}
	return fmt.Sprintf("%s (+%d)", it.paths[0], len(it.paths)-1)
}

// logEntry is one line of the activity feed, stamped so a slow LLM round is
// visibly slow rather than merely absent.
type logEntry struct {
	// stamp is the pre-formatted clock reading. Formatting it once here keeps
	// the render path from re-laying out the same timestamp on every frame.
	stamp string
	text  string
	kind  Kind
	// count is how many events this row stands for. It is 1 for an ordinary
	// line and greater when a run of identically-reasoned skips has been
	// collapsed into this row.
	count int
	// reason is the skip reason a collapsed row aggregates.
	reason string
}

// tickMsg drives the spinner and the clock.
type tickMsg time.Time

// eventMsg carries a published Event into the Bubble Tea update loop. The
// pipeline's goroutines send these from outside; Program.Send is documented as
// safe to call concurrently, which is what makes this the only locking-free
// path from a worker goroutine to the view.
type eventMsg struct{ Event }

// model is the dashboard. It is a value type, as Bubble Tea requires; all
// mutable state is either a pointer field or a slice the Update loop replaces.
type model struct {
	title    string
	subtitle string
	model    string
	provider string
	repo     string

	items   []*item
	byGroup map[string]*item

	log []logEntry
	// logOffset is how far the visible window has been scrolled back from the
	// tail. Zero means following.
	logOffset int
	// pending counts lines recorded while the log was paused, so releasing the
	// pause can resume following at the true tail rather than where it was.
	pending int

	totalFiles   int
	running      int
	reusedFiles  int
	doneFiles    int
	failedFiles  int
	skippedFiles int
	findings     int
	budgetHit    bool

	startedAt time.Time
	// color mirrors the caller's resolved --color decision. False renders the
	// dashboard without any ANSI styling.
	color bool
	// tokens reports the run's cumulative usage. It is polled on the render
	// tick rather than pushed: the per-group breakdown already arrives as
	// events, and the run-wide total is two atomics away from whoever owns
	// them.
	tokens  func() (input, output int64)
	session func() string
	// inputTokens and outputTokens are the last polled totals.
	inputTokens  int64
	outputTokens int64
	// sessionID is the last polled id, empty until the session persists.
	sessionID string
	paused    bool
	detached  bool
	// focus is which pane the arrow keys scroll. The work pane holds the
	// concurrent rows, the activity pane the log; both can outgrow their
	// region, so they cannot both be driven by one pair of keys.
	focus   pane
	workTop int
	// showCompleted expands the finished rows. They are folded away by
	// default because a long scan finishes hundreds of files and the finished
	// ones are not what the reader is watching for.
	showCompleted bool

	width  int
	height int

	spinner spinner.Model
	// cancel abandons the run when the user presses ctrl+c.
	cancel func()
}

// newModel seeds the dashboard. totalFiles is the frozen coverage denominator
// the run already computed, so the bar is meaningful from the first frame.
func newModel(o Options, totalFiles int) model {
	sp := spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(styleBusy))
	return model{
		color:      o.Color,
		tokens:     o.Tokens,
		session:    o.Session,
		title:      o.Title,
		subtitle:   o.Subtitle,
		model:      o.Model,
		provider:   o.Provider,
		repo:       o.Repo,
		byGroup:    map[string]*item{},
		focus:      paneWork,
		totalFiles: totalFiles,
		startedAt:  time.Now(),
		spinner:    sp,
		cancel:     o.Cancel,
		width:      80,
		height:     24,
	}
}

// Init starts the spinner and the clock.
func (m model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, tick())
}

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// Update folds one message into the dashboard.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Clamp to a usable minimum. A terminal can legitimately report 0
		// (a pty with no size yet, or the window between a close and the
		// resize that follows), and every region clips itself to the frame
		// width — so a zero would blank the whole dashboard rather than
		// merely look wrong.
		m.width = max(msg.Width, minFrameWidth)
		m.height = max(msg.Height, minFrameHeight)
		return m, nil

	case tickMsg:
		if m.tokens != nil {
			m.inputTokens, m.outputTokens = m.tokens()
		}
		if m.session != nil {
			m.sessionID = m.session()
		}
		return m, tick()

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case eventMsg:
		m.apply(msg.Event)
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		// Detach, do not cancel. The run is already in flight on other
		// goroutines; quitting the view must not abandon it or lose its
		// results. The runner swaps back to the text sink on the way out, so
		// progress after this point simply returns to the terminal as the
		// [ocr] lines it used to be.
		m.detached = true
		return m, tea.Quit

	case "ctrl+c":
		// Raw mode clears the kernel's ISIG bit, so no SIGINT is delivered
		// while this view is up and the run's signal handler never fires.
		// The dashboard therefore has to hand the request back to the run
		// itself, which is the only thing that knows how to shut down
		// gracefully. The view stays up until the run returns, so the
		// shutdown is visible rather than instantaneous.
		if m.cancel != nil {
			m.cancel()
		}
		return m, nil

	case "p":
		// Pausing freezes the log window where it is. It does not stop the
		// run: pausing is for reading, not for throttling work.
		m.paused = !m.paused
		if !m.paused {
			m.logOffset = 0
			m.pending = 0
		}
		return m, nil

	case "c":
		// Fold the finished rows away or back out. On a long scan they
		// outnumber the running ones and push them off the screen.
		m.showCompleted = !m.showCompleted
		m.workTop = 0
		return m, nil

	case "tab", "shift+tab":
		m.focus = oppositePane(m.focus)
		return m, nil

	case "u", "home":
		// Snap back to the tail of whatever is focused.
		m.logOffset = 0
		m.workTop = 0
		return m, nil

	case "j", "down":
		m.scrollPane(1)
		return m, nil

	case "k", "up":
		m.scrollPane(-1)
		return m, nil
	}
	return m, nil
}

func oppositePane(p pane) pane {
	if p == paneWork {
		return paneActivity
	}
	return paneWork
}

// scrollPane moves the focused pane one step. delta is +1 for "further
// forward" (down the list) and -1 for "back" (up it).
//
// The two panes count in opposite directions, which is why the sign is
// flipped for the log: a work row's offset grows as you move down the list,
// while the log's offset is measured backwards from the tail, so moving down
// towards the newest line shrinks it. Normalising here keeps every caller
// thinking in one direction.
func (m *model) scrollPane(delta int) {
	if m.focus == paneActivity {
		m.scroll(-delta)
		return
	}
	m.workTop += delta
	if m.workTop < 0 {
		m.workTop = 0
	}
	if last := m.workRows() - 1; m.workTop > last {
		m.workTop = last
	}
}

// scroll moves the activity window. delta is positive for older entries.
// Reaching the tail clears the hold, so a reader who scrolls to the bottom
// starts following again without a second keypress.
func (m *model) scroll(delta int) {
	m.logOffset += delta
	if m.logOffset <= 0 {
		m.logOffset = 0
	}
}

// holding reports whether the reader has scrolled the activity log away from
// the tail.
func (m model) holding() bool { return m.logOffset > 0 }

// apply folds one event into the dashboard state. It is the only writer, so
// the counters and the row list can never disagree.
func (m *model) apply(ev Event) {
	switch ev.Kind {
	case KindGroupStart:
		if m.startItem(ev) {
			m.running++
		}

	case KindRoundStart:
		if it := m.lookup(ev.Group); it != nil {
			it.round = ev.Round
		}

	case KindGroupDone:
		m.doneFiles += m.finish(ev, stateDone)
		m.findings += ev.Findings

	case KindGroupFailed:
		m.failedFiles += m.finish(ev, stateFailed)

	case KindSkipped:
		m.skippedFiles++
		m.appendSkip(ev)
		return

	case KindBudgetReached:
		m.budgetHit = true

	case KindPlanned:
		m.totalFiles = ev.Files

	case KindToolStart:
		if it := m.lookup(ev.Group); it != nil {
			it.tool = ev.Detail
		}

	case KindReused:
		// Reused files count as covered but have no row: nothing is being
		// done to them now, and listing hundreds of them would bury the work.
		// Their findings still count — they are real findings this run is
		// going to report, found by the run it is resuming.
		m.reusedFiles += ev.Files
		m.findings += ev.Findings

	case KindUsage:
		// Cumulative totals from the pipeline, not deltas: the row shows what
		// the group has spent in total, which is also what the header sums.
		if it := m.lookup(ev.Group); it != nil {
			it.tokens = ev.InputTokens + ev.OutputTokens
		}
	}

	// The log is the complete record: every kind lands here, because a warning
	// is exactly the line a user needs to be able to scroll back to. Events
	// that carry no text at all (the coverage denominator) are the exception —
	// they are state, not something to say.
	//
	// A paused dashboard keeps recording, so nothing is lost while the user
	// reads; what pausing does is stop the visible window following the tail,
	// which is what would otherwise drag their line off the bottom.
	text := strings.TrimRight(ev.Text, "\n")
	if text == "" {
		return
	}
	if m.paused {
		// Hold the visible window where the reader left it. The entry is
		// still recorded, so nothing is lost — the count in the heading keeps
		// climbing, and releasing the pause brings it into view.
		m.pending++
	}
	m.log = append(m.log, logEntry{stamp: ev.Time.Format("15:04:05"), text: text, kind: ev.Kind})
	if len(m.log) > logRetained {
		m.log = m.log[len(m.log)-logRetained:]
	}
}

// appendSkip adds a skipped file to the activity log, collapsing a run of
// files skipped for the same reason into one row. The event's own per-file
// text is deliberately not logged separately: printing it as well would give
// every skipped file two rows, and the per-file row sitting between two skips
// would break the run this collapses.
//
// Selection can skip hundreds of files for one of a handful of reasons, and
// printing one log line each buries everything else: on a real scan the first
// thing the reader saw was 272 identical-looking lines and none of the lines
// that explain what the run was about to do. The collapse is TUI-only — the
// per-file lines are still printed for piped and CI output, where they are the
// record of what was left out.
func (m *model) appendSkip(ev Event) {
	reason := ev.SkipReason
	if n := len(m.log); n > 0 {
		last := &m.log[n-1]
		if last.kind == KindSkipped && last.reason == reason {
			last.count++
			last.text = fmt.Sprintf("Skipping %d file(s) — %s", last.count, reason)
			return
		}
	}
	m.log = append(m.log, logEntry{
		stamp:  ev.Time.Format("15:04:05"),
		text:   fmt.Sprintf("Skipping %s — %s", ev.Detail, reason),
		kind:   KindSkipped,
		count:  1,
		reason: reason,
	})
}

// finish closes out a work row and returns how many files it accounts for.
// Done and failed differ only in the state and the one extra field, so they
// share this rather than repeating the same ten lines twice.
func (m *model) finish(ev Event, state itemState) int {
	it := m.lookup(ev.Group)
	if it == nil {
		// The row is missing (a resumed run, a late event). The files still
		// have to be counted, or the bar would stall short of full.
		return max(ev.Files, 1)
	}
	if it.state == stateRunning {
		m.running--
	}
	it.state = state
	it.tool = ""
	it.took = time.Since(it.started)
	if state == stateDone {
		it.findings = ev.Findings
	} else {
		it.err = ev.Err
	}
	return m.fileCount(ev, it)
}

// startItem creates or refreshes a work row, reporting whether it is new.
// A repeat announcement is a resumed group being re-announced: it must refresh
// the row's clock, not stack a duplicate or count as extra concurrent work.
func (m *model) startItem(ev Event) bool {
	if it, ok := m.byGroup[ev.Group]; ok {
		it.started = time.Now()
		return false
	}
	it := &item{label: ev.Group, paths: ev.Paths, state: stateRunning, started: time.Now()}
	m.byGroup[ev.Group] = it
	m.items = append(m.items, it)
	return true
}

func (m *model) lookup(group string) *item {
	if group == "" {
		return nil
	}
	return m.byGroup[group]
}

// fileCount is how many files a finished group accounts for. It prefers the
// event's own count, falls back to the group's recorded paths, and never
// reports zero: a dispatched group always covers at least one file, and
// undercounting would stall the bar short of 100% on a run that finished.
func (m *model) fileCount(ev Event, it *item) int {
	if ev.Files > 0 {
		return ev.Files
	}
	if len(it.paths) > 0 {
		return len(it.paths)
	}
	return 1
}

// fraction is the bar's progress, over the files selected for review. See
// finishedFiles for why skipped files are not in the numerator.
func (m model) fraction() float64 {
	if m.totalFiles <= 0 {
		return 0
	}
	return float64(m.finishedFiles()) / float64(m.totalFiles)
}
