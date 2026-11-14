package ecfr

import "sort"

// maxLCSCells bounds the table used to line paragraphs up. Past it, every
// paragraph between the unchanged start and end of a division is reported
// as removed and added: still correct, only less fine. A variable so a test
// can lower it.
var maxLCSCells = 1 << 20

// change builds the report entry for one division. old is nil for an added
// division and new is nil for a removed one; both carry detail.
func change(old, new *unit) Change {
	switch {
	case new == nil:
		return Change{Where: where(old.key), Kind: KindRemoved,
			AmountsRemoved: sorted(old.amounts), DatesRemoved: sorted(old.dates)}
	case old == nil:
		return Change{Where: where(new.key), Kind: KindAdded,
			AmountsAdded: sorted(new.amounts), DatesAdded: sorted(new.dates)}
	}
	c := Change{Where: where(old.key), Kind: KindModified}
	if old.head != new.head {
		c.Fields = append(c.Fields, FieldHeading)
	}
	if old.body != new.body {
		c.Fields = append(c.Fields, FieldText)
		c.Paragraphs = diffBlocks(old.blocks, new.blocks)
	}
	c.AmountsRemoved, c.AmountsAdded = multisetDiff(old.amounts, new.amounts)
	c.DatesRemoved, c.DatesAdded = multisetDiff(old.dates, new.dates)
	return c
}

func sorted(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	c := append([]string{}, s...)
	sort.Strings(c)
	return c
}

// multisetDiff returns what is in a and not b, and what is in b and not a,
// counting repeats, each sorted.
func multisetDiff(a, b []string) (onlyA, onlyB []string) {
	count := map[string]int{}
	for _, s := range a {
		count[s]++
	}
	for _, s := range b {
		if count[s] > 0 {
			count[s]--
		} else {
			onlyB = append(onlyB, s)
		}
	}
	// What is left in count is only in a. Walk a to keep the result stable.
	for _, s := range a {
		if count[s] > 0 {
			count[s]--
			onlyA = append(onlyA, s)
		}
	}
	return sorted(onlyA), sorted(onlyB)
}

// diffBlocks lines up two lists of paragraphs and reports the ones removed
// (by position in the old division) and added (by position in the new).
// Positions count from 1. An edited paragraph shows as one removed and one
// added.
func diffBlocks(a, b []block) []ParagraphChange {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre].sum == b[pre].sum {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf].sum == b[len(b)-1-suf].sum {
		suf++
	}
	am, bm := a[pre:len(a)-suf], b[pre:len(b)-suf]
	var out []ParagraphChange
	removed := func(i int) {
		out = append(out, ParagraphChange{Op: KindRemoved, Position: pre + i + 1, Marker: am[i].marker})
	}
	added := func(j int) {
		out = append(out, ParagraphChange{Op: KindAdded, Position: pre + j + 1, Marker: bm[j].marker})
	}
	if len(am) == 0 || len(bm) == 0 || (len(am)+1)*(len(bm)+1) > maxLCSCells {
		for i := range am {
			removed(i)
		}
		for j := range bm {
			added(j)
		}
		return out
	}
	// lcs[i*w+j] is the longest common run of am[i:] and bm[j:].
	w := len(bm) + 1
	lcs := make([]int32, (len(am)+1)*w)
	for i := len(am) - 1; i >= 0; i-- {
		for j := len(bm) - 1; j >= 0; j-- {
			switch {
			case am[i].sum == bm[j].sum:
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
				lcs[i*w+j] = lcs[(i+1)*w+j]
			default:
				lcs[i*w+j] = lcs[i*w+j+1]
			}
		}
	}
	i, j := 0, 0
	for i < len(am) && j < len(bm) {
		switch {
		case am[i].sum == bm[j].sum:
			i++
			j++
		case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			removed(i)
			i++
		default:
			added(j)
			j++
		}
	}
	for ; i < len(am); i++ {
		removed(i)
	}
	for ; j < len(bm); j++ {
		added(j)
	}
	return out
}
