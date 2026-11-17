package qr

import "strings"

// quietZone is the light margin a reader needs around a code, in modules.
const quietZone = 4

// Black on bright white, whatever the terminal's own colours, so that a dark terminal doesn't draw
// the code inverted.
const (
	colours = "\x1b[30;107m"
	reset   = "\x1b[0m"
)

// Terminal draws c in Unicode half blocks, two rows of modules to a line, with its quiet zone, and
// returns the lines.
func (c *Code) Terminal() []string {
	var lines []string
	for row := -quietZone; row < c.Size+quietZone; row += 2 {
		var b strings.Builder
		b.WriteString(colours)
		for col := -quietZone; col < c.Size+quietZone; col++ {
			top, bottom := c.Dark(row, col), c.Dark(row+1, col)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteString(reset)
		lines = append(lines, b.String())
	}
	return lines
}
