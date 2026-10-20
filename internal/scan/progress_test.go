// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package scan

import (
	"strings"
	"testing"

	"github.com/alibaba/open-code-review/internal/model"
	"github.com/alibaba/open-code-review/internal/progress"
)

// The scan's progress events are the dashboard's only view of a running scan.
// A publisher that nothing calls leaves the bar with no denominator, the work
// list empty, and the counters at zero, so each one is exercised here.

// captureProgress collects the events fn publishes.
func captureProgress(t *testing.T, fn func()) []progress.Event {
	t.Helper()
	var got []progress.Event
	restore := progress.SetSinkForTest(func(ev progress.Event) {
		got = append(got, ev)
	})
	defer restore()
	fn()
	return got
}

func TestPublishPlannedSetsTheCoverageDenominator(t *testing.T) {
	got := captureProgress(t, func() { publishPlanned(7) })

	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].Kind != progress.KindPlanned {
		t.Errorf("kind = %v, want KindPlanned", got[0].Kind)
	}
	if got[0].Files != 7 {
		t.Errorf("files = %d, want 7", got[0].Files)
	}
}

func TestPublishItemLifecycle(t *testing.T) {
	it := model.ScanItem{Path: "a.go"}

	start := captureProgress(t, func() { publishItemStart(it) })
	if len(start) != 1 {
		t.Fatalf("expected 1 event, got %d", len(start))
	}
	if start[0].Kind != progress.KindGroupStart {
		t.Errorf("kind = %v, want KindGroupStart", start[0].Kind)
	}
	if start[0].Group != "a.go" {
		t.Errorf("group = %q, want the item's path", start[0].Group)
	}

	done := captureProgress(t, func() { publishItemDone(it, 3) })
	if len(done) != 1 {
		t.Fatalf("expected 1 event, got %d", len(done))
	}
	if done[0].Kind != progress.KindGroupDone {
		t.Errorf("kind = %v, want KindGroupDone", done[0].Kind)
	}
	if done[0].Findings != 3 {
		t.Errorf("findings = %d, want 3", done[0].Findings)
	}

	failed := captureProgress(t, func() { publishItemFailed(it, "boom") })
	if len(failed) != 1 {
		t.Fatalf("expected 1 event, got %d", len(failed))
	}
	if failed[0].Kind != progress.KindGroupFailed {
		t.Errorf("kind = %v, want KindGroupFailed", failed[0].Kind)
	}
	if failed[0].Err != "boom" {
		t.Errorf("err = %q, want the failure reason", failed[0].Err)
	}
}

func TestPublishBudgetReached(t *testing.T) {
	got := captureProgress(t, func() { publishBudgetReached() })

	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].Kind != progress.KindBudgetReached {
		t.Errorf("kind = %v, want KindBudgetReached", got[0].Kind)
	}
}

// The dashboard's log rows are the events' own text, and the text sink adds
// the "[ocr] " prefix itself. A scan line that carried its own prefix would
// show it twice.
func TestScanProgressTextHasNoPrefix(t *testing.T) {
	got := captureProgress(t, func() { progress.Noticef("a scan line\n") })

	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if strings.HasPrefix(got[0].Text, "[ocr] ") {
		t.Errorf("scan line carries the text sink's prefix: %q", got[0].Text)
	}
}
