package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// helpWidth is what help's prose wraps at when the stream has no width of its own, as when piped.
const helpWidth = 80

// Command is what a command's help says of it.
type Command struct {
	// Name is how it's run, without its arguments: "oikade deploy", "oikade-host add".
	Name string
	// Summary is what it does, in a line, for the list of commands.
	Summary string
	// Usage is a line for each way to run it: "oikade deploy [--module DIR | --binary FILE]". A flag's
	// value is named here, such as DIR, and its help says it that way.
	Usage []string
	// About is more on what it does, if a line isn't enough.
	About string
	// Example is a line or two of a command someone would run.
	Example []string
	// Values name the values of flags the usage doesn't, such as {"data-dir": "DIR"}.
	Values map[string]string
	// Hide is flags that aren't listed, such as gone ones kept to say where they went.
	Hide []string
	// Commands are the binary's others, listed after the flags, as oikade-host's help lists add.
	Commands []Command
}

// Word is the command's last word, as oikade help takes it: "deploy", "add".
func (c Command) Word() string {
	return c.Name[strings.LastIndexByte(c.Name, ' ')+1:]
}

// HelpFlags makes fs print c's help to stdout for -h or --help, and exit 0. A flag fs doesn't take,
// or a bad value, prints "Error: …" and c's usage to stderr, and exits ExitUsage. exit is os.Exit,
// except in tests.
func HelpFlags(fs *flag.FlagSet, c Command, stdout, stderr *Printer, exit func(int)) {
	var failed bytes.Buffer
	fs.SetOutput(&failed)
	fs.Usage = func() {
		if failed.Len() == 0 {
			stdout.CommandHelp(c, fs)
			exit(0)
			return
		}
		message := FlagError(c, fs, strings.TrimSpace(failed.String()))
		failed.Reset()
		exit(stderr.FailUsage(UsageErrorf("%s", message), false, c))
	}
}

var (
	undefinedFlag = regexp.MustCompile(`^flag provided but not defined: -+(.+)$`)
	noValue       = regexp.MustCompile(`^flag needs an argument: -+(.+)$`)
	badBool       = regexp.MustCompile(`^invalid boolean value "(.*)" for -+(\S+): .*$`)
	badValue      = regexp.MustCompile(`^invalid value "(.*)" for flag -+(\S+): (.*)$`)
)

// FlagError is what the flag package says of a bad flag of c's fs, in the user's words.
func FlagError(c Command, fs *flag.FlagSet, message string) string {
	if m := undefinedFlag.FindStringSubmatch(message); m != nil {
		return fmt.Sprintf("%s has no flag --%s", c.Name, m[1])
	}
	if m := noValue.FindStringSubmatch(message); m != nil {
		return fmt.Sprintf("--%s needs a value", m[1])
	}
	if m := badBool.FindStringSubmatch(message); m != nil {
		return fmt.Sprintf("--%s is true or false, not %q", m[2], m[1])
	}
	if m := badValue.FindStringSubmatch(message); m != nil {
		if m[3] != "parse error" && !strings.Contains(m[3], "invalid syntax") && !strings.Contains(m[3], "out of range") {
			return m[3]
		}
		kind := ""
		if f := fs.Lookup(m[2]); f != nil {
			kind, _ = flag.UnquoteUsage(f)
		}
		switch kind {
		case "duration":
			return fmt.Sprintf("--%s is a duration, such as 90s or 10m, not %q", m[2], m[1])
		case "int", "uint", "int64", "uint64", "float":
			return fmt.Sprintf("--%s is a number, not %q", m[2], m[1])
		}
		return fmt.Sprintf("--%s doesn't take %q", m[2], m[1])
	}
	return message
}

// FailUsage prints err, as Fail does, then c's usage, and err's hint or one naming c's help. It
// returns the code to exit with.
func (p *Printer) FailUsage(err error, debug bool, c Command) int {
	hint := c.Name + " --help says more"
	if e, ok := errors.AsType[*Error](err); ok && e.Hint != "" {
		hint = e.Hint
	}
	code := p.fail(err, debug, false)
	p.Println(p.Bold("Usage:"))
	for _, u := range c.Usage {
		p.Println("  " + u)
	}
	p.Hint("%s", hint)
	return code
}

// CommandHelp prints c's help: what it does, its usage, fs's flags in --flag VALUE form, and an
// example.
func (p *Printer) CommandHelp(c Command, fs *flag.FlagSet) {
	p.Println(p.wrap(c.Name+": "+c.Summary, 0))
	if c.About != "" {
		p.Println()
		p.Println(p.wrap(c.About, 0))
	}
	p.Println()
	p.Println(p.Bold("Usage:"))
	for _, u := range c.Usage {
		p.Println("  " + u)
	}
	if flags := flagsOf(c, fs); len(flags) > 0 {
		p.Println()
		p.Println(p.Bold("Flags:"))
		column := 0
		for _, f := range flags {
			column = max(column, 2+width(f.name)+gap)
		}
		for _, f := range flags {
			lead := "  " + f.name + strings.Repeat(" ", column-2-width(f.name))
			p.Println(lead + p.wrap(f.usage, column))
		}
	}
	if len(c.Commands) > 0 {
		p.Println()
		p.Println(p.Bold("Commands:"))
		p.commandList(c.Name, c.Commands, listColumn(c.Name, c.Commands))
		p.Println()
		p.Println(p.wrap(c.Name+" COMMAND --help says more of one.", 0))
	}
	if len(c.Example) > 0 {
		p.Println()
		p.Println(p.Bold("Example:"))
		for _, e := range c.Example {
			p.Println("  " + e)
		}
	}
}

// listedFlag is a flag as help lists it: "--module DIR", and what it's for.
type listedFlag struct {
	name, usage string
}

// valueName is a flag in a usage line, with the name of its value: "--module DIR".
var valueName = regexp.MustCompile(`--([a-z][a-z0-9-]*)(?: ([A-Z][A-Z0-9_]*))?`)

// flagsOf is fs's flags, those c's usage names first, in its order, then the rest by name. Each is
// named as the usage names it, or for its type.
func flagsOf(c Command, fs *flag.FlagSet) []listedFlag {
	values := maps.Clone(c.Values)
	if values == nil {
		values = map[string]string{}
	}
	var order []string
	for _, m := range valueName.FindAllStringSubmatch(strings.Join(c.Usage, " "), -1) {
		if !slices.Contains(order, m[1]) {
			order = append(order, m[1])
		}
		if m[2] != "" || values[m[1]] == "" {
			values[m[1]] = m[2]
		}
	}
	var all []*flag.Flag
	fs.VisitAll(func(f *flag.Flag) {
		if !slices.Contains(c.Hide, f.Name) {
			all = append(all, f)
		}
	})
	rank := func(f *flag.Flag) int {
		if i := slices.Index(order, f.Name); i >= 0 {
			return i
		}
		return len(order)
	}
	slices.SortStableFunc(all, func(a, b *flag.Flag) int { return rank(a) - rank(b) })

	listed := make([]listedFlag, 0, len(all))
	for _, f := range all {
		name := "--" + f.Name
		if v := valueNameOf(f, values[f.Name]); v != "" {
			name += " " + v
		}
		usage := f.Usage
		if def := shortDuration(f.DefValue); !isBool(f) && def != "" && def != "0" && def != "0s" {
			usage += fmt.Sprintf(" (default %s)", def)
		}
		listed = append(listed, listedFlag{name, usage})
	}
	return listed
}

// valueNameOf is what a flag's value is called: as the usage line calls it, else for its type, and
// nothing for a flag that's true or false.
func valueNameOf(f *flag.Flag, named string) string {
	if isBool(f) {
		return ""
	}
	if named != "" {
		return named
	}
	kind, _ := flag.UnquoteUsage(f)
	switch kind {
	case "duration":
		return "DURATION"
	case "int", "uint", "uint64", "int64", "float":
		return "N"
	}
	return "VALUE"
}

// shortDuration is a duration's default without its zero parts, 336h rather than 336h0m0s, and
// anything else as it is.
func shortDuration(v string) string {
	if _, err := time.ParseDuration(v); err != nil {
		return v
	}
	if strings.HasSuffix(v, "m0s") {
		v = strings.TrimSuffix(v, "0s")
	}
	if strings.HasSuffix(v, "h0m") {
		v = strings.TrimSuffix(v, "0m")
	}
	return v
}

func isBool(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// wrap breaks text into lines that fit the printer, or helpWidth on a stream with no width, when
// each starts at column indent: the first after what's already on its line, the rest after spaces.
func (p *Printer) wrap(text string, indent int) string {
	limit := p.Width
	if limit <= 0 {
		limit = helpWidth
	}
	var lines []string
	line := ""
	for word := range strings.FieldsSeq(text) {
		switch {
		case line == "":
			line = word
		case indent+width(line)+1+width(word) > limit:
			lines = append(lines, line)
			line = word
		default:
			line += " " + word
		}
	}
	return strings.Join(append(lines, line), "\n"+strings.Repeat(" ", indent))
}

// Closest is the one of names that word is most likely a slip for, or "" if none is close.
func Closest(word string, names []string) string {
	best, bestDistance := "", 3
	for _, name := range names {
		d := distance(word, name)
		if strings.HasPrefix(name, word) && len(word) >= 3 {
			d = min(d, 1)
		}
		if d < bestDistance {
			best, bestDistance = name, d
		}
	}
	return best
}

// distance is the Levenshtein distance between a and b.
func distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	row := make([]int, len(rb)+1)
	for j := range row {
		row[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		prev := row[0]
		row[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			prev, row[j] = row[j], min(row[j]+1, row[j-1]+1, prev+cost)
		}
	}
	return row[len(rb)]
}

// Group is commands that go together in a binary's help, under a title such as "Ship it".
type Group struct {
	Title    string
	Commands []Command
}

// Overview prints a binary's help: its usage line, then each group's commands with what they do,
// then the paragraphs after.
func (p *Printer) Overview(binary, usage string, groups []Group, after ...string) {
	p.Println(p.Bold("Usage:") + " " + usage)
	var all []Command
	for _, g := range groups {
		all = append(all, g.Commands...)
	}
	column := listColumn(binary, all)
	for _, g := range groups {
		p.Println()
		p.Println(p.Bold(g.Title))
		p.commandList(binary, g.Commands, column)
	}
	for _, a := range after {
		p.Println()
		p.Println(p.wrap(a, 0))
	}
}

// listColumn is where a list of commands' summaries start, after the longest name.
func listColumn(binary string, commands []Command) int {
	column := 0
	for _, c := range commands {
		column = max(column, 2+width(strings.TrimPrefix(c.Name, binary+" "))+gap)
	}
	return column
}

// commandList lists commands, each by its name without binary's, with its summary at column.
func (p *Printer) commandList(binary string, commands []Command, column int) {
	for _, c := range commands {
		name := strings.TrimPrefix(c.Name, binary+" ")
		p.Println("  " + name + strings.Repeat(" ", column-2-width(name)) + p.wrap(c.Summary, column))
	}
}
