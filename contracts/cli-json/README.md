# CLI `--json` output schemas

Versioned JSON Schema files for every CLI command's `--json` output, one file per command (see
`docs/architecture.md` §4 and §10, and `CLAUDE.md` rule 15). A schema change ships in the same commit as
the code change that causes it, the same rule as `openapi.yaml` and `gateway-events.schema.json`.

This directory was expected to stay empty until Milestone M48, which is where the roadmap puts the first
data-printing command. M10 arrived earlier: `norite instance invite` prints invite codes, and rule 15 does
not have a milestone attached to it.

M20 added the first verbs relayed through the daemon as the signed-in account, one schema per group, each
verb's output a definition in it that the CLI's tests validate against.

| File | Commands |
| --- | --- |
| `instance-invite.schema.json` | `norite instance invite create \| list \| revoke` |
| `common.schema.json` | `done`, what a verb prints when the instance answered with no object, and `user`, another account |
| `guild.schema.json` | `norite guild list \| create \| show \| update \| delete \| transfer \| audit-log \| recording-log` |
| `channel.schema.json` | `norite channel list \| create \| update \| delete` |
| `role.schema.json` | `norite role list \| create \| update \| reorder \| delete` |
| `member.schema.json` | `norite member list \| update \| remove \| role add \| role remove` |
| `overwrite.schema.json` | `norite overwrite set \| delete` |
| `message.schema.json` | `norite message list \| send \| edit \| delete \| history` |
| `report.schema.json` | `norite report file \| list \| show \| resolve \| dismiss` |
| `tag.schema.json` | `norite tag list \| create \| delete \| apply \| unapply \| on` |
| `guild-invite.schema.json` | `norite invite create \| list \| show \| join \| revoke` |
| `about.schema.json` | `norite about` |

**These shapes belong to the CLI, not to the instance.** Several of them are built from a REST response
carrying the same information, and they are re-declared here rather than passed through: a scripted
caller's input must not change because an instance renamed a field. Where the two agree today, that is a
fact about today.

Conventions worth keeping, the first two set by the first schema:

- **A nullable field is present and explicitly `null`, never omitted.** A caller reads the same keys
  whichever kind of value it got, so `jq '.max_uses'` answers rather than failing.
- **A list prints `[]` when empty, never `null`.** Go marshals a nil slice to `null`, so this is something
  each command has to do on purpose.
- **A list that pages is an object**, `{"items": [...], "next": "<id>" | null}`, where `next` is what to pass
  to the verb's paging flag for the following page and is null on the last. There is no `--all`: a script
  that wants everything loops on `next`.
- **A verb whose request returns no object prints `done`**, naming what it did and to what, rather than
  nothing — `instance invite revoke`'s precedent.
- **Every string an instance sent is exactly as sent**, with what a terminal would act on escaped as
  `\uXXXX` by the CLI's one JSON writer: lossless to a parser, inert on a terminal (rule 19).
