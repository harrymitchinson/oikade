package qr

import "strings"

// quietZone is the light margin a reader needs around a code, in modules.
const quietZone = 4

// Terminal draws c in Unicode half blocks, two rows of modules to a line, with its quiet zone, and
// returns the lines: dark modules are the blocks, to draw in black on white (cli.Image).
func (c *Code) Terminal() []string {
	var lines []string
	for row := -quietZone; row < c.Size+quietZone; row += 2 {
		var b strings.Builder
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
		lines = append(lines, b.String())
	}
	return lines
}
