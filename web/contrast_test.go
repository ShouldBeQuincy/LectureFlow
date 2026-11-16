// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.

package web

import (
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// N41: the text colours of style.css reach WCAG AA (4.5:1) on the
// backgrounds they are drawn on, light and dark. testdata/contrast_test.mjs
// parses the token block and lists the pairs with what each one is on screen.
func TestTextContrast(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed: the contrast checks of testdata/contrast_test.mjs need it")
	}
	out, err := runNode(t, node, "contrast_test.mjs", "style.css")
	if err != nil || !strings.Contains(string(out), "contrast checks: ok") {
		t.Fatalf("contrast_test.mjs: %v\n%s", err, out)
	}
}

// rgbaColour is a colour of the token block: 0–255 channels and an alpha.
type rgbaColour struct {
	c [3]float64
	a float64
}

var (
	reHex6 = regexp.MustCompile(`^#([0-9a-fA-F]{2})([0-9a-fA-F]{2})([0-9a-fA-F]{2})$`)
	reRGBA = regexp.MustCompile(`^rgb\((\d+) (\d+) (\d+) / ([\d.]+)\)$`)
)

func parseColour(t *testing.T, s string) rgbaColour {
	t.Helper()
	s = strings.TrimSpace(s)
	if m := reHex6.FindStringSubmatch(s); m != nil {
		var out rgbaColour
		for i := 0; i < 3; i++ {
			v, _ := strconv.ParseUint(m[i+1], 16, 8)
			out.c[i] = float64(v)
		}
		out.a = 1
		return out
	}
	if m := reRGBA.FindStringSubmatch(s); m != nil {
		var out rgbaColour
		for i := 0; i < 3; i++ {
			out.c[i], _ = strconv.ParseFloat(m[i+1], 64)
		}
		out.a, _ = strconv.ParseFloat(m[4], 64)
		return out
	}
	t.Fatalf("unknown colour %q", s)
	return rgbaColour{}
}

// lightDarkToken returns the light and the dark value of a light-dark()
// token of the token block of style.css.
func lightDarkToken(t *testing.T, css, name string) (light, dark rgbaColour) {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(name) + `:\s*light-dark\(([^,]+),\s*([^;]+)\);`).FindStringSubmatch(css)
	if m == nil {
		t.Fatalf("style.css has no light-dark() token %s", name)
	}
	return parseColour(t, m[1]), parseColour(t, m[2])
}

func overColour(top, base rgbaColour) [3]float64 {
	var out [3]float64
	for i := range out {
		out[i] = top.c[i]*top.a + base.c[i]*(1-top.a)
	}
	return out
}

func relLuminance(c [3]float64) float64 {
	var l [3]float64
	for i, v := range c {
		v /= 255
		if v <= 0.04045 {
			l[i] = v / 12.92
		} else {
			l[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*l[0] + 0.7152*l[1] + 0.0722*l[2]
}

func contrastRatio(a, b [3]float64) float64 {
	x, y := relLuminance(a), relLuminance(b)
	return (math.Max(x, y) + 0.05) / (math.Min(x, y) + 0.05)
}

func hueOf(c [3]float64) float64 {
	r, g, b := c[0]/255, c[1]/255, c[2]/255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	d := mx - mn
	if d == 0 {
		return 0
	}
	var h float64
	switch mx {
	case r:
		h = math.Mod((g-b)/d+6, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h * 60
}

// R4 (NOTES "4.16"): the hint of a highlighted context menu item (its
// shortcut or count, .ctx-hint) sits on --bg-active (focus, open submenu)
// or --bg-hover over --bg-raised. In the dark theme --text-3 there was
// 3.59 / 4.16:1; the highlighted item's hint has a lighter grey of the same
// hue, at least 4.5:1 on both, light and dark. The light theme keeps
// --text-3 (4.83 / 5.19). Computed here, so it holds without node too.
func TestMenuHintContrast(t *testing.T) {
	css := readWeb(t, "style.css")
	if got := cssRule(t, css, ".ctx-hint"); !strings.Contains(got, "color: var(--text-3);") {
		t.Errorf(".ctx-hint at rest is no longer --text-3: %q", got)
	}
	rule := cssRule(t, css, `.ctx-item:focus .ctx-hint, .ctx-item[aria-expanded="true"] .ctx-hint`)
	m := regexp.MustCompile(`color: light-dark\(var\(--text-3\), (#[0-9a-fA-F]{6})\);`).FindStringSubmatch(rule)
	if m == nil {
		t.Fatalf("the highlighted menu item's hint is not light-dark(var(--text-3), #rrggbb): %q", rule)
	}
	if hl := cssRule(t, css, `.ctx-item:focus, .ctx-item[aria-expanded="true"]`); !strings.Contains(hl, "background: var(--bg-active);") {
		t.Fatalf("the highlighted menu item is no longer --bg-active; update the check: %q", hl)
	}
	darkHint := parseColour(t, m[1])
	t3l, t3d := lightDarkToken(t, css, "--text-3")
	raisedL, raisedD := lightDarkToken(t, css, "--bg-raised")
	activeL, activeD := lightDarkToken(t, css, "--bg-active")
	hoverL, hoverD := lightDarkToken(t, css, "--bg-hover")
	for _, c := range []struct {
		what   string
		fg     rgbaColour
		bg, on rgbaColour
	}{
		{"light, --bg-active", t3l, activeL, raisedL},
		{"light, --bg-hover", t3l, hoverL, raisedL},
		{"dark, --bg-active", darkHint, activeD, raisedD},
		{"dark, --bg-hover", darkHint, hoverD, raisedD},
	} {
		if r := contrastRatio(c.fg.c, overColour(c.bg, c.on)); r < 4.5 {
			t.Errorf("menu hint on the highlighted item (%s over --bg-raised) is %.2f:1, below 4.5", c.what, r)
		}
	}
	// the same grey, only lighter: the hue of the dark --text-3 (the accent
	// blue and the colour tokens stay as they are)
	if d := math.Abs(hueOf(darkHint.c) - hueOf(t3d.c)); d > 1 {
		t.Errorf("the highlighted hint %s has hue %.1f, the dark --text-3 %.1f", m[1], hueOf(darkHint.c), hueOf(t3d.c))
	}
	if relLuminance(darkHint.c) <= relLuminance(t3d.c) {
		t.Errorf("the highlighted hint %s is not lighter than the dark --text-3", m[1])
	}
}
