package main

import (
	"strings"

	"oikade.dev/internal/cli"
	"oikade.dev/internal/ident"
)

// kind is what oikade help KIND says of a kind of thing: how it's written, and the commands that
// take one. TestEveryVerbExitsAsTheGrammarSays holds each command to it.
type kind struct {
	// word is the kind as oikade help takes it, singular. Its plural works too.
	word string
	// about says how a thing of the kind is written.
	about string
	// commands are those that take one, each written as it's run, and what it does with it.
	commands []cli.Command
}

// happening is what tail does with one thing.
const happening = "What it does, as it happens."

// kinds are what oikade help KIND knows, in the order a home has them.
var kinds = []kind{
	{"device", "A device is device/NAME, or its name alone for status, history, tail and forget, " +
		"which take a device first.", []cli.Command{
		{Name: "oikade list devices", Summary: "What's on record, and how each is set up."},
		{Name: "oikade status device/NAME", Summary: "What it's doing now, why, and its last 10 events."},
		{Name: "oikade history device/NAME", Summary: "What it did, and who wrote to it."},
		{Name: "oikade tail device/NAME", Summary: happening},
		{Name: "oikade forget device/NAME", Summary: "Drop it from the host's record, and end its claims."},
	}},
	{"capability", "A capability is device/DEVICE/CAPABILITY, such as device/porch-light/on.", []cli.Command{
		{Name: "oikade status device/DEVICE/CAPABILITY", Summary: "Its value, and why it's that."},
		{Name: "oikade history device/DEVICE/CAPABILITY", Summary: "What it did, and who wrote to it."},
		{Name: "oikade tail device/DEVICE/CAPABILITY", Summary: happening},
		{Name: "oikade set device/DEVICE/CAPABILITY VALUE", Summary: "Set it as a person would, through arbitration."},
	}},
	{"room", "A room is room/NAME, and stands for the devices in it now.", []cli.Command{
		{Name: "oikade status room/NAME", Summary: "What its devices are doing now."},
		{Name: "oikade history room/NAME", Summary: "What its devices did."},
		{Name: "oikade tail room/NAME", Summary: "What its devices do, as it happens."},
	}},
	{"case", "A case is case/DEVICE/CAPABILITY/CASE, a line of a decision, as oikade status names it.", []cli.Command{
		{Name: "oikade list rules", Summary: "Every rule, and whether it's paused."},
		{Name: "oikade status case/DEVICE/CAPABILITY/CASE", Summary: "Whether it applies, and why not."},
		{Name: "oikade history case/DEVICE/CAPABILITY/CASE", Summary: "What it wrote, and its pauses."},
		{Name: "oikade tail case/DEVICE/CAPABILITY/CASE", Summary: happening},
		{Name: "oikade pause case/DEVICE/CAPABILITY/CASE", Summary: "Stop it, so it lets go of what it holds."},
		{Name: "oikade resume case/DEVICE/CAPABILITY/CASE", Summary: "Start it again."},
	}},
	{"flow", "A flow is flow/NAME.", []cli.Command{
		{Name: "oikade list rules", Summary: "Every rule, and whether it's paused."},
		{Name: "oikade status flow/NAME", Summary: "Whether it's running, and what it holds."},
		{Name: "oikade history flow/NAME", Summary: "Its runs, the writes it made, and its pauses."},
		{Name: "oikade tail flow/NAME", Summary: happening},
		{Name: "oikade pause flow/NAME", Summary: "Stop it, so it lets go of what it holds."},
		{Name: "oikade resume flow/NAME", Summary: "Start it again."},
	}},
	{"plugin", "A plugin is plugin/NAME, or its name alone for setup, which takes a plugin first.", []cli.Command{
		{Name: "oikade list plugins", Summary: "Each with its problem, and whether it needs setup."},
		{Name: "oikade status plugin/NAME", Summary: "Its problem, whether it needs setup, and its devices."},
		{Name: "oikade history plugin/NAME", Summary: "Its own problems, such as a refused login."},
		{Name: "oikade tail plugin/NAME", Summary: "Its problems, as they happen."},
		{Name: "oikade setup plugin/NAME", Summary: "Give it what it needs, such as a sign-in."},
		{Name: "oikade forget --plugin NAME", Summary: "Forget every device it reported."},
	}},
	{"client", "A client is client/NAME, a machine that uses the host's network API.", []cli.Command{
		{Name: "oikade list clients", Summary: "Each with when it was added, and those waiting for their login."},
		{Name: "oikade remove client/NAME", Summary: "Remove it, so its token stops working."},
		{Name: "oikade-host add client/NAME", Summary: "Add one, on the host's machine."},
	}},
	{"context", "A context is a host this machine logged in to. Every command that talks to a host takes " +
		"--context NAME.", []cli.Command{
		{Name: "oikade list contexts", Summary: "Each, with the current one marked."},
		{Name: "oikade login", Summary: "Trust a host's network API, and save it as a context."},
	}},
	{string(ident.Matter), "matter is the Matter bridge, so a device called matter is device/matter.", []cli.Command{
		{Name: "oikade status matter", Summary: "What the bridge is doing, and who's paired."},
		{Name: "oikade history matter", Summary: "Its window, pairings, and what controllers wrote."},
		{Name: "oikade tail matter", Summary: "The same, as it happens."},
		{Name: "oikade pair matter", Summary: "Open its window to pair a controller."},
	}},
}

// kindNamed is the kind a word names, singular or plural, as in oikade help devices.
func kindNamed(word string) (kind, bool) {
	for _, k := range kinds {
		if word == k.word || word == k.word+"s" {
			return k, true
		}
	}
	return kind{}, false
}

// kindWords are the kinds oikade help takes.
func kindWords() []string {
	words := make([]string, len(kinds))
	for i, k := range kinds {
		words[i] = k.word
	}
	return words
}

// takes says whether the command word takes a ref of kind k, as its help says: list takes a kind's
// word instead, and forget a plugin's name with --plugin.
func (k kind) takes(word string) bool {
	for _, c := range k.commands {
		fields := strings.Fields(c.Name)
		if len(fields) > 2 && fields[0] == "oikade" && fields[1] == word &&
			(strings.Contains(fields[2], "/") || fields[2] == string(ident.Matter)) {
			return true
		}
	}
	return false
}

// printKind prints oikade help KIND: how a thing of the kind is written, and the commands that take it.
func printKind(p *cli.Printer, k kind) {
	p.Println(p.Wrap(k.about))
	p.Println()
	title := "Commands that take a " + k.word
	if k.word == string(ident.Matter) {
		title = "Commands that take matter"
	}
	p.Println(p.Bold(title))
	p.CommandList("oikade", k.commands)
	p.Println()
	p.Println(p.Wrap("oikade help COMMAND says more of one."))
}
