package cli

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

// Exit codes.
const (
	// ExitFailed is a command that went wrong.
	ExitFailed = 1
	// ExitUsage is a command given the wrong arguments or flags.
	ExitUsage = 2
)

// Error is what went wrong in the user's words, the command or flag that fixes it, and the cause,
// which only --debug shows.
type Error struct {
	// Message is one sentence, without "Error:".
	Message string
	// Hint is the command or flag that fixes it, if there's one.
	Hint string
	// Usage is a command given the wrong arguments or flags, which exits ExitUsage.
	Usage bool
	// Err is the cause, such as a Go error chain.
	Err error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Message
	}
	return e.Message + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Errorf is an Error saying what went wrong, as fmt.Sprintf.
func Errorf(format string, a ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, a...)}
}

// Wrap is an Error saying what went wrong, as fmt.Sprintf, caused by err, which only --debug shows.
func Wrap(err error, format string, a ...any) *Error {
	return &Error{Message: fmt.Sprintf(format, a...), Err: err}
}

// UsageErrorf is an Error for a command given the wrong arguments or flags, which exits ExitUsage.
func UsageErrorf(format string, a ...any) *Error {
	e := Errorf(format, a...)
	e.Usage = true
	return e
}

// WithHint is e with a hint.
func (e *Error) WithHint(format string, a ...any) *Error {
	e.Hint = fmt.Sprintf(format, a...)
	return e
}

// Debug takes --debug (or --debug=BOOL) from a command's arguments, and says whether it or
// OIKADE_DEBUG asks for errors in full. It stops at "--", and for a command in passOn, which hands
// what follows its flags to another program, as oikade test does to go test, at the first word
// after the command's.
func Debug(args []string, passOn ...string) (rest []string, debug bool) {
	debug, _ = strconv.ParseBool(os.Getenv("OIKADE_DEBUG"))
	command := ""
	for i, arg := range args {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "-"), "=")
		passing := command != "" && slices.Contains(passOn, command) && !strings.HasPrefix(arg, "-")
		switch {
		case arg == "--" || passing:
			return append(rest, args[i:]...), debug
		case name == "-debug" || name == "debug":
			debug = true
			if hasValue {
				debug, _ = strconv.ParseBool(value)
			}
			continue
		case command == "" && !strings.HasPrefix(arg, "-"):
			command = arg
		}
		rest = append(rest, arg)
	}
	return rest, debug
}

// Fail prints err, "Error: …" and any hint, and returns the code to exit with: ExitUsage for an
// Error with Usage, and ExitFailed otherwise. An Error prints its Message, and its cause only if
// debug asks for it. Any other error prints in full.
func (p *Printer) Fail(err error, debug bool) int {
	code := ExitFailed
	message, hint, full := err.Error(), "", ""
	if e, ok := errors.AsType[*Error](err); ok {
		message, hint = e.Message, e.Hint
		if e.Usage {
			code = ExitUsage
		}
		if debug && err.Error() != message {
			full = err.Error()
		}
	}
	p.Println(p.glyph("✗", p.Red) + p.Red("Error:") + " " + message)
	if hint != "" {
		p.Hint("%s", hint)
	}
	if full != "" {
		p.Println(p.Dim("Debug: " + full))
	}
	return code
}
