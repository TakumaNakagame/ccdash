package model

import (
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
)

// PickColor draws a palette color that stands apart from used (the colors
// of the sessions running now): it scores each candidate by its distance to
// the nearest used color and picks at random among the best — a re-roll
// that avoids both identical and look-alike colors. avoid (e.g. the
// session's current color) is never returned unless it is the only option.
func PickColor(used []string, avoid string) string {
	type cand struct {
		c     string
		score float64
	}
	var cs []cand
	best := -1.0
	for _, c := range SessionPalette {
		if strings.EqualFold(c, avoid) {
			continue
		}
		score := 1000.0
		for _, u := range used {
			score = math.Min(score, colorDist(c, u))
		}
		cs = append(cs, cand{c, score})
		best = math.Max(best, score)
	}
	if len(cs) == 0 {
		return SessionPalette[0]
	}
	// Anything within a few degrees of the best counts as equally good, so
	// a re-roll still varies.
	var top []string
	for _, x := range cs {
		if x.score >= best-8 {
			top = append(top, x.c)
		}
	}
	return top[rand.IntN(len(top))]
}

// colorDist is a rough perceptual distance between two "#rrggbb" colors:
// hue difference in degrees plus lightness difference. Greys have no
// meaningful hue, so they are as far from every hue as a quarter turn.
func colorDist(a, b string) float64 {
	ha, sa, la, ok1 := hsl(a)
	hb, sb, lb, ok2 := hsl(b)
	if !ok1 || !ok2 {
		return 1000
	}
	var dh float64
	switch {
	case sa < 0.25 && sb < 0.25:
		dh = 0
	case sa < 0.25 || sb < 0.25:
		dh = 90
	default:
		dh = math.Abs(ha - hb)
		if dh > 180 {
			dh = 360 - dh
		}
	}
	return dh + math.Abs(la-lb)*100
}

// hsl converts "#rrggbb" to hue (degrees), saturation and lightness (0..1).
func hsl(c string) (h, s, l float64, ok bool) {
	if !ValidColor(c) {
		return 0, 0, 0, false
	}
	v, _ := strconv.ParseUint(c[1:], 16, 32)
	r, g, b := float64(v>>16&0xff)/255, float64(v>>8&0xff)/255, float64(v&0xff)/255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l = (mx + mn) / 2
	d := mx - mn
	if d == 0 {
		return 0, 0, l, true
	}
	s = d / (1 - math.Abs(2*l-1))
	switch mx {
	case r:
		h = math.Mod((g-b)/d, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	return h, s, l, true
}
