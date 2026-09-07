# Contributing

Norite is free software under the [AGPL-3.0-or-later](LICENSE), and it is **not currently open to code
contributions**. That combination is unusual enough to deserve a reason rather than a policy line, so this
file gives the reason — and says what *is* welcome, because that part is not nothing.

## What is welcome

- **Bug reports.** Open an issue. What you did, what happened, what you expected, and the version or commit
  if you know it.
- **Questions and discussion** about the design. The reasoning behind the contested decisions lives in
  [`docs/adr/`](docs/adr/) and the full design in [`docs/architecture.md`](docs/architecture.md); if
  something there is wrong, unclear, or contradicts the code, saying so is genuinely useful.
- **Forks.** The license grants this outright. Fork it, run it, modify it, host it. The only conditions are
  the AGPL's own — offer your source to the users of any modified network service you run — plus the name
  reservation under §7(e): don't ship a fork or a derivative service under the Norite name and branding in
  a way likely to cause confusion.

**Security issues go to [`SECURITY.md`](SECURITY.md), never to a public issue.**

## Why pull requests are not accepted

Two different reasons, and only the first is permanent.

**For `backend/`, it is a licensing constraint.** The copyright holder currently owns 100% of the code,
which means the backend can be relicensed at will — dual-licensed, or granted a commercial exception, or
moved to something permissive — as long as it stays free of both copyleft dependencies and un-assigned
contributions. CI enforces the dependency half. Nothing can enforce the other half except not merging.

A single un-assigned patch to `backend/` ends that option **permanently**, because undoing it would require
the agreement of every copyright holder. The option is not a plan and may never be exercised; it costs
nothing to keep and cannot be recovered once spent. That asymmetry is the whole argument, and it is
recorded in [ADR 0032](docs/adr/0032-agpl-license.md).

The clients are already past this point by design: `daemon/` takes `go.mau.fi/libsignal` (GPL-3.0) at M97,
after which that binary is permanently copyleft-locked including for the copyright holder. So the constraint
above genuinely is backend-specific rather than a blanket preference.

**Everywhere else, it is review capacity.** This is a solo project with a long dependency-ordered roadmap
and a set of non-negotiable engineering rules that a reviewer has to hold in their head. Reviewing an
outside patch properly costs more than writing it. That is a practical limit, not a principled one, and it
is revisitable — if the project reaches a point where the review capacity exists, this section changes.

## The grey area worth naming

A short bug report is not a copyrightable contribution in any sense that matters. **A substantial patch
pasted into an issue and then copied into `backend/` is exactly the same contamination as a merged pull
request** — the route it took in does not change who holds copyright in it.

So: describe the bug, and describe the fix in prose if you have one. Please don't paste a diff for
`backend/`. If a report does arrive with a patch attached, the fix will be reimplemented independently
rather than copied, which is slower for everyone and is the reason to mention it here first.

## If a contribution process ever opens

It will start with a CLA covering `backend/`, and this file will say so. Until then, treating a pull
request as unwelcome is not the intent — the intent is not to waste your time on work that cannot be
merged.
