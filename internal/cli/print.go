package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// DefaultWidth is a table's width on a terminal that doesn't say its own.
const DefaultWidth = 80

// The styles, in SGR codes. Only a Styled printer writes them.
const (
	sgrBold   = "1"
	sgrDim    = "2"
	sgrRed    = "31"
	sgrGreen  = "32"
	sgrYellow = "33"
	// sgrImage is black on bright white, whatever the terminal's own colours, so that a dark terminal
	// doesn't draw an image such as a QR code inverted.
	sgrImage = "30;107"
)

// Printer writes what a command says to one stream, stdout or stderr, in the look every oikade and
// oikade-host command shares: plain text that reads right anywhere, with colour and glyphs only for
// a person at a terminal.
type Printer struct {
	w io.Writer
	// Terminal is a stream a person reads, on a terminal that isn't dumb, where a line can be redrawn
	// and an image drawn.
	Terminal bool
	// Styled is a Terminal without NO_COLOR, which gets colour and glyphs.
	Styled bool
	// Width is the stream's, in columns, which tables keep to. Zero is no limit.
	Width int
}

// NewPrinter prints to f, as its terminal, NO_COLOR, TERM and COLUMNS say.
func NewPrinter(f *os.File) *Printer {
	fd := int(f.Fd())
	terminal := term.IsTerminal(fd)
	width := 0
	if terminal {
		width, _, _ = term.GetSize(fd)
	}
	return newPrinter(f, terminal, width, os.Getenv)
}

// newPrinter prints to w, a terminal of width columns or not, as the environment getenv reads says.
// A terminal that doesn't say its width is DefaultWidth wide, unless COLUMNS says. Anything else,
// such as a pipe, has no limit, so that grep finds what a table holds.
func newPrinter(w io.Writer, terminal bool, width int, getenv func(string) string) *Printer {
	if width <= 0 {
		width, _ = strconv.Atoi(getenv("COLUMNS"))
	}
	if width <= 0 && terminal {
		width = DefaultWidth
	}
	terminal = terminal && getenv("TERM") != "dumb"
	return &Printer{w: w, Terminal: terminal, Styled: terminal && getenv("NO_COLOR") == "", Width: max(width, 0)}
}

// Plain prints to w with no colour or glyphs, at width columns, as to a file or in a test.
func Plain(w io.Writer, width int) *Printer {
	return &Printer{w: w, Width: width}
}

// Styled prints to w as to a terminal of width columns, with colour and glyphs.
func Styled(w io.Writer, width int) *Printer {
	return &Printer{w: w, Terminal: true, Styled: true, Width: width}
}

// Write writes p as it is, so that a Printer is an io.Writer.
func (p *Printer) Write(b []byte) (int, error) {
	return p.w.Write(b)
}

// Println writes a line, as fmt.Println.
func (p *Printer) Println(a ...any) {
	_, _ = fmt.Fprintln(p.w, a...)
}

// Printf writes, as fmt.Printf.
func (p *Printer) Printf(format string, a ...any) {
	_, _ = fmt.Fprintf(p.w, format, a...)
}

func (p *Printer) style(sgr, s string) string {
	if !p.Styled || s == "" {
		return s
	}
	return "\x1b[" + sgr + "m" + s + "\x1b[0m"
}

// Bold is for a table's header and a section's title.
func (p *Printer) Bold(s string) string { return p.style(sgrBold, s) }

// Dim is for what's secondary: refs, hints, "not in your code".
func (p *Printer) Dim(s string) string { return p.style(sgrDim, s) }

// Green is for what's done.
func (p *Printer) Green(s string) string { return p.style(sgrGreen, s) }

// Yellow is for a warning.
func (p *Printer) Yellow(s string) string { return p.style(sgrYellow, s) }

// Red is for a failure.
func (p *Printer) Red(s string) string { return p.style(sgrRed, s) }

// Image paints a line of an image, such as a QR code's, black on bright white, for a Terminal
// whether or not it's Styled, as it's a picture and not a style.
func Image(line string) string {
	return "\x1b[" + sgrImage + "m" + line + "\x1b[0m"
}

// glyph is g and a space before a line, on a Styled printer.
func (p *Printer) glyph(g string, colour func(string) string) string {
	if !p.Styled {
		return ""
	}
	return colour(g) + " "
}

// Done says what a command did: "✓ Deployed version f113018".
func (p *Printer) Done(format string, a ...any) {
	p.Println(p.glyph("✓", p.Green) + fmt.Sprintf(format, a...))
}

// Warn says what someone should know but needn't stop for: "Warning: …".
func (p *Printer) Warn(format string, a ...any) {
	p.Println(p.glyph("!", p.Yellow) + p.Yellow("Warning:") + " " + fmt.Sprintf(format, a...))
}

// Hint names the command or flag that does more, or fixes what went wrong: "Hint: …".
func (p *Printer) Hint(format string, a ...any) {
	p.Println(p.Dim("Hint: " + fmt.Sprintf(format, a...)))
}

// width is s's in columns, which is its runes' count, as each is one column in what oikade prints.
func width(s string) int {
	return utf8.RuneCountInString(s)
}

// shorten fits s into w columns, ending it with an ellipsis if it doesn't.
func shorten(s string, w int) string {
	if width(s) <= w {
		return s
	}
	if w <= 0 {
		return ""
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:w-1]), " ,") + "…"
}
