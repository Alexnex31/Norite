<!-- ---------------------------------------------------------------------------
     Norite is free software (AGPL-3.0-or-later) but is NOT open to outside code
     contributions. Pull requests from anyone other than the copyright holder
     cannot be merged — for backend/ this is a permanent licensing constraint,
     not a preference. Please read CONTRIBUTING.md before spending time on a
     patch; bug reports and design discussion in issues are welcome, and a
     security issue goes to SECURITY.md rather than to a public issue.

     This template has exactly the three sections below. Do not add a fourth;
     anything else goes inside "What does this change?" as prose or sub-bullets.
     --------------------------------------------------------------------------- -->

## What does this change?

<!-- Summary of the change and why. Link the issue/milestone it belongs to, if any. -->

## Checklist

- [ ] I read `CLAUDE.md`'s non-negotiable rules and this PR complies with the ones that apply
- [ ] If this adds/changes a REST endpoint: `contracts/openapi.yaml` updated in this PR (see `/new-endpoint`)
- [ ] If this adds/changes a gateway event: `contracts/gateway-events.schema.json` + frontend Zod schema
      updated in this PR (see `/new-gateway-event`)
- [ ] If this adds a mutating endpoint: it checks permissions before writing, and writes an audit log entry
      if guild-scoped
- [ ] If this changes the schema: the migration includes any index the new query shape needs (see
      `/db-migration`)
- [ ] Tests added/updated (unit + integration/E2E as applicable) and passing locally
- [ ] `just lint` passes
- [ ] If this touches auth/permissions/gateway/content-rendering: ran through `/security-audit`

## How was this tested?

<!-- Commands run, manual verification steps, screenshots for UI changes. -->
