# Matter test attestation credentials

These are connectedhomeip's published test credentials, copied unchanged from
[project-chip/connectedhomeip](https://github.com/project-chip/connectedhomeip) at
`credentials/test/attestation` and `credentials/test/certification-declaration`
(Apache License 2.0, Copyright Project CHIP Authors).

- `Chip-Test-PAA-FFF1-Cert.pem`: the test PAA for vendor 0xFFF1
- `Chip-Test-PAI-FFF1-8000-Cert.pem`: a PAI under it, for product 0x8000
- `Chip-Test-DAC-FFF1-8000-0000-Cert.pem` and its key: a DAC under the PAI
- `Chip-Test-CD-Signing-Cert.pem` and its key: the test Certification Declaration signer

The keys are public test material, not secrets. A commissioner that trusts the test PAA reports a
node attested with them as uncertified.
