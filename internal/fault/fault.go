// Package fault marks the points where a crash matters, so tests can crash there rather than hope
// to hit them with timing. They do nothing unless built with the faultpoints tag, which only the
// tests use.
package fault

// Fault points. A journal point counts entries and finishes alike.
const (
	// ClientJournalBefore is your process about to send, and ClientJournalAfter it holding the
	// answer.
	ClientJournalBefore = "client.journal.before"
	ClientJournalAfter  = "client.journal.after"
	// HostJournalBefore is the host about to write, and HostJournalAfter the entry durable and
	// not yet answered.
	HostJournalBefore = "host.journal.before"
	HostJournalAfter  = "host.journal.after"
	// HostJournalAppend is the write itself, which a test can drop to prove it's what keeps a step
	// from running twice.
	HostJournalAppend = "host.journal.append"
	// HostClaimReplied is the host having answered a claim.
	HostClaimReplied = "host.claim.replied"
	// HostDeployConfirmed is a new version confirmed, with its removed rules not yet dealt with.
	HostDeployConfirmed = "host.deploy.confirmed"
	// HostDeployChecked is a new version checked, with the old one still running.
	HostDeployChecked = "host.deploy.checked"
	// HostDeployBegun is a new version recorded as unconfirmed, with the old one still running.
	HostDeployBegun = "host.deploy.begun"
	// HostRemovals is the host about to end what removed rules left, which a test can drop to make
	// that fail.
	HostRemovals = "host.removals"
	// HostEndRule is the host about to end a removed rule's claims, which a test can drop to make
	// that fail.
	HostEndRule = "host.removals.end"
)
