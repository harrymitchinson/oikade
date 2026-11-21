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
	// ClientHoldStarted is your process having written or claimed a hold's start, and not yet
	// saved anything after it.
	ClientHoldStarted = "client.hold.started"
	// HostJournalBefore is the host about to write, and HostJournalAfter the entry durable and
	// not yet answered.
	HostJournalBefore = "host.journal.before"
	HostJournalAfter  = "host.journal.after"
	// HostJournalAppend is the write itself, which a test can drop to prove it's what keeps a step
	// from running twice.
	HostJournalAppend = "host.journal.append"
	// HostClaimReplied is the host having answered a claim, which it saved first.
	HostClaimReplied = "host.claim.replied"
	// HostDeploySettled is a new version settled and not yet confirmed.
	HostDeploySettled = "host.deploy.settled"
	// HostDeployConfirmed is a new version confirmed, with its removed rules not yet dealt with.
	HostDeployConfirmed = "host.deploy.confirmed"
	// HostDeployBegun is a new version recorded as unconfirmed, with the old one still running.
	HostDeployBegun = "host.deploy.begun"
	// HostRemovals is the host about to end what removed rules left, which a test can drop to make
	// that fail.
	HostRemovals = "host.removals"
	// HostEndRule is the host about to end a removed rule's claims, which a test can drop to make
	// that fail.
	HostEndRule = "host.removals.end"
	// RegistrySaved is a change to the device registry on disk, not yet taken up or answered.
	RegistrySaved = "registry.saved"
	// HostOptionsSaved is a setting of the home's, such as the bridge's name, on disk and not yet
	// applied.
	HostOptionsSaved = "host.options.saved"
	// HostFabricsSaved is the Matter fabric table on disk, with the command that changed it not yet
	// answered.
	HostFabricsSaved = "host.fabrics.saved"
	// HostSecretSaved is a secret on disk, not yet answered.
	HostSecretSaved = "host.secret.saved"
	// HostUploadBefore is a deploy's upload received and not yet moved among the versions, and
	// HostUploadAfter it moved, with no version yet naming it.
	HostUploadBefore = "host.upload.before"
	HostUploadAfter  = "host.upload.after"
)
