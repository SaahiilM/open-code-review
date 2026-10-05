// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package progress

import (
	"fmt"
	"strings"
)

// Token counts run into the millions on a large scan, and a fixed-width frame
// has no room for that in full. The abbreviations follow the convention other
// system monitors use (btop, htop, btop's own token column): keep one decimal
// while the leading digit is single, drop it once there are two or more, and
// step up to M at a million.
//
//	380      -> "380"      999      -> "999"
//	1_000    -> "1k"       3_800    -> "3.8k"
//	12_400   -> "12k"      123_000  -> "123k"
//	1_050_000 -> "1.1M"     12_400_000 -> "12M"
func humanTokens(n int64) string {
	switch {
	case n < 0:
		return "0"
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 10_000:
		return trimZero(float64(n)/1000) + "k"
	case n < 1_000_000:
		return fmt.Sprintf("%dk", n/1000)
	case n < 10_000_000:
		return trimZero(float64(n)/1_000_000) + "M"
	default:
		return fmt.Sprintf("%dM", n/1_000_000)
	}
}

// trimZero renders a float with one decimal place and drops a trailing ".0"
// or bare ".", so the column does not jitter between "4k" and "4.0k" as a
// count crosses a boundary.
func trimZero(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}

// humanDuration is the same idea for elapsed time: seconds below a minute,
// then m:ss, so a row's two columns stay narrow and comparable.
func humanDuration(seconds int64) string {
	switch {
	case seconds < 0:
		return "0s"
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
	default:
		return fmt.Sprintf("%dh%02dm", seconds/3600, (seconds%3600)/60)
	}
}
