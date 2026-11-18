package cli

import (
	"strings"
)

// gap is the space between a table's columns.
const gap = 2

// minShrunk is the fewest columns a cell is shortened to, or its header's width if that's more.
const minShrunk = 12

// Column is one of a Table's.
type Column struct {
	Header string
	// Drop is when the column goes, if the table is too wide for the stream: 1 goes first, then 2,
	// and 0 never goes. --wide keeps them all.
	Drop int
	// Shrink lets its cells be shortened, with an ellipsis, once no column is left to go.
	Shrink bool
	// Dim is for a column of what's secondary, such as refs.
	Dim bool
	// DimIf dims the cells it's true of, in a column where only some cells are secondary.
	DimIf func(cell string) bool
}

// Table is rows under a bold header, fitted to the stream's width. An empty cell stays blank.
type Table struct {
	Columns []Column
	Rows    [][]string
	// Wide keeps every column at its full width, however wide the stream.
	Wide bool
}

// Table prints t, and returns the headers of the columns that didn't fit, for a hint that --wide
// shows them.
func (p *Printer) Table(t Table) (dropped []string) {
	widths := make([]int, len(t.Columns))
	for i, c := range t.Columns {
		widths[i] = width(c.Header)
		for _, row := range t.Rows {
			if i < len(row) {
				widths[i] = max(widths[i], width(row[i]))
			}
		}
	}
	shown := make([]bool, len(t.Columns))
	for i := range shown {
		shown[i] = true
	}
	if !t.Wide && p.Width > 0 {
		dropped = fit(t.Columns, widths, shown, p.Width)
	}

	last := -1
	for i, s := range shown {
		if s {
			last = i
		}
	}
	var b strings.Builder
	line := func(cells []string, style func(int, string) string) {
		b.Reset()
		for i := range t.Columns {
			if !shown[i] {
				continue
			}
			cell := ""
			if i < len(cells) {
				cell = shorten(cells[i], widths[i])
			}
			b.WriteString(style(i, cell))
			if i == last {
				break
			}
			b.WriteString(strings.Repeat(" ", widths[i]-width(cell)+gap))
		}
		p.Println(strings.TrimRight(b.String(), " "))
	}
	headers := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		headers[i] = c.Header
	}
	line(headers, func(_ int, s string) string { return p.Bold(s) })
	for _, row := range t.Rows {
		line(row, func(i int, s string) string {
			if c := t.Columns[i]; c.Dim || c.DimIf != nil && c.DimIf(s) {
				return p.Dim(s)
			}
			return s
		})
	}
	return dropped
}

// fit drops columns, by their Drop, and then shortens the Shrink ones, widest first, until the
// table is no wider than limit or nothing more can give. It returns the headers it dropped.
func fit(columns []Column, widths []int, shown []bool, limit int) (dropped []string) {
	total := func() int {
		sum := 0
		for i, w := range widths {
			if shown[i] {
				sum += w + gap
			}
		}
		return sum - gap
	}
	for total() > limit {
		next := -1
		for i, c := range columns {
			if shown[i] && c.Drop > 0 && (next < 0 || c.Drop < columns[next].Drop) {
				next = i
			}
		}
		if next < 0 {
			break
		}
		shown[next] = false
		dropped = append(dropped, columns[next].Header)
	}
	for over := total() - limit; over > 0; over-- {
		widest := -1
		for i, c := range columns {
			if shown[i] && c.Shrink && widths[i] > max(minShrunk, width(c.Header)) &&
				(widest < 0 || widths[i] > widths[widest]) {
				widest = i
			}
		}
		if widest < 0 {
			break
		}
		widths[widest]--
	}
	return dropped
}
