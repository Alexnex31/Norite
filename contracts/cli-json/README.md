# CLI `--json` output schemas

Versioned JSON Schema files for every CLI command's `--json` output, one file per command (see
`docs/architecture.md` §4 and §10, and `CLAUDE.md` rule 15). A schema change ships in the same commit as
the code change that causes it, the same rule as `openapi.yaml` and `gateway-events.schema.json`.

This directory was expected to stay empty until Milestone M48, which is where the roadmap puts the first
data-printing command. M10 arrived earlier: `norite instance invite` prints invite codes, and rule 15 does
not have a milestone attached to it.

| File | Commands |
| --- | --- |
| `instance-invite.schema.json` | `norite instance invite create \| list \| revoke` |

**These shapes belong to the CLI, not to the instance.** Several of them are built from a REST response
carrying the same information, and they are re-declared here rather than passed through: a scripted
caller's input must not change because an instance renamed a field. Where the two agree today, that is a
fact about today.

Two conventions the first schema sets, worth keeping:

- **A nullable field is present and explicitly `null`, never omitted.** A caller reads the same keys
  whichever kind of value it got, so `jq '.max_uses'` answers rather than failing.
- **A list prints `[]` when empty, never `null`.** Go marshals a nil slice to `null`, so this is something
  each command has to do on purpose.
