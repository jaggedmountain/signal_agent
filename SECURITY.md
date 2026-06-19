# Security Policy

## Reporting a vulnerability

If you find a security issue in this code, please open a public GitHub issue. Include
a short description, reproduction steps, and (if relevant) the version /
commit you tested against. We'll respond within a few days and coordinate a
fix and disclosure timeline with you.

For non-security bugs and feature requests, use GitHub issues normally.

## Scope

In scope:

- The Go agent in this repo (`signal_agent.go`).
- The shipped systemd unit (`signal_agent.service`).
- The documented configuration surface (`README.md`, generated
  `signal_agent.env`).

Out of scope (report upstream):

- `signal-cli` itself — file at https://github.com/AsamK/signal-cli.
- The Claude CLI / Claude Code — file with Anthropic.
- The Signal protocol or Signal app behavior.

## Threat model

See [Threat model](README.md#threat-model) in the README for the day-to-day
operational posture (who is trusted, what `/!!` means, what the journal
leaks). The short version: **whoever can post to Signal Note-to-Self
thread can run code on the host**. The agent is a remote-execution channel
guarded only by Signal's account-level security.

If the phone is compromised — physically taken, SIM-swapped, or the Signal
account otherwise hijacked — the agent on the linked host should be assumed
compromised too. Mitigations:

- Stop the service: `systemctl --user stop signal_agent.service`.
- Unlink the device from the phone (Settings → Linked Devices) so future
  notes are not delivered to the host even if the service is restarted.
- Rotate Anthropic credentials at `~/.claude/.credentials.json` if `/!!`
  history suggests a third party reached the host.
