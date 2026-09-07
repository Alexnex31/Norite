# Contributing

Norite is free software under the [AGPL-3.0-or-later](LICENSE), and contributions are welcome.

**Issues, bug reports and questions need no formality at all** — open one. That includes telling us the
documentation is wrong, which for a project this early is often the most useful thing anybody can do: the
reasoning behind the contested decisions is in [`docs/adr/`](docs/adr/) and the full design in
[`docs/architecture.md`](docs/architecture.md), and if something there contradicts the code, saying so is a
real contribution.

**Security issues go to [`SECURITY.md`](SECURITY.md), never to a public issue.**

Norite is built by one person, so **please open an issue before a large pull request.** A change that does
not fit the direction in `docs/architecture.md` is much better discussed than written — that is a courtesy
to your own time more than to mine.

## What a pull request needs

It depends on which module you touch, because the two halves of this repository carry different licensing
constraints. Everything here is about **who can make licensing decisions later**, never about who wrote
what: contributors are credited either way, in the commit history and in the release notes.

### `daemon/`, `cli/`, `gui/` — a sign-off

Add a `Signed-off-by` line to each commit, which `git commit -s` does for you:

```
Signed-off-by: Your Name <your.email@example.com>
```

That line certifies the Developer Certificate of Origin, reproduced in full below. **You keep your
copyright.** That is the whole ask, and it is the same one the Linux kernel and a great many other projects
make.

### `backend/` — a copyright assignment as well

The backend additionally requires a signed copyright assignment before a patch can be merged.

This is a real ask and it deserves the real reason. The backend deliberately carries **no copyleft
dependency** — end-to-end encryption is client-side by design, so the server relays ciphertext it cannot
read and never needs `go.mau.fi/libsignal` or anything like it. As long as that holds *and* every line is
held by one copyright holder, the backend's license remains an open question that can still be answered:
dual-licensed, granted a commercial exception, or moved to something permissive. A single contribution held
by somebody else closes that permanently, because reopening it would need the agreement of every holder.

The daemon is already past that point by design and deliberately so — it takes `go.mau.fi/libsignal`
(GPL-3.0) at `M97`, after which that binary is permanently copyleft-locked including for the copyright
holder. So this constraint really is backend-specific rather than a blanket preference, and
[ADR 0032](docs/adr/0032-agpl-license.md) section 8 is where the whole argument lives.

**The exact instrument and how it gets signed are not settled yet**, and it would be dishonest to publish a
legal document that has not been reviewed. If you want to contribute to `backend/`, open an issue or say so
in your pull request and the agreement will be provided then. Nothing is being hidden — there is simply no
contributor yet, and the text will be written properly rather than improvised against a deadline.

If that is more paperwork than a small fix is worth to you: **report it as an issue instead.** A precise bug
report costs you nothing legally, and for a one-line fix it is genuinely the better trade.

### One grey area, worth naming

A short bug report is not a copyrightable contribution in any sense that matters, and describing a fix in
prose is not either. **A substantial patch pasted into an issue and then copied into `backend/` is the same
contamination as merging it**, though — the route it took in does not change who holds copyright.

So for `backend/` specifically: describe the bug, and describe the fix in prose if you have one, but please
don't paste a diff unless you are ready to sign the assignment. If one arrives anyway it will be
reimplemented independently rather than copied, which is slower for everybody and is why this is worth
saying up front. For the client modules the sign-off covers it and this does not apply.

## Before you push

The engineering rules that actually get a change rejected are in [`CLAUDE.md`](CLAUDE.md) — the
non-negotiable list is short and each rule says why it exists. The ones that come up most:

```sh
just lint         # zero issues, every module
just test         # the full suite with -race, the same gate CI runs
just test-short   # unit tests only, no container runtime needed
```

A new REST endpoint updates `contracts/openapi.yaml` in the same commit; a new gateway event updates
`contracts/gateway-events.schema.json`; a new hot-path query ships with the index it needs. Commit messages
follow [Conventional Commits](https://www.conventionalcommits.org/) — read `git log` for the house style
before writing one.

## Developer Certificate of Origin 1.1

```
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.


Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same license (unless I am permitted to submit
    under a different license), as indicated in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```
