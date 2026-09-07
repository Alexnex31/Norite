<!-- ---------------------------------------------------------------------------
     Contributions are welcome. What a PR needs depends on the module it touches:

       daemon/, cli/, gui/  a DCO sign-off — `git commit -s`. You keep your
                            copyright. That is the whole ask.
       backend/             a signed copyright assignment as well. It is the one
                            module whose licensing is still an open question,
                            and one patch held elsewhere would close it for
                            good. Say so in the PR and the agreement will be
                            provided — the text is not drafted yet.

     CONTRIBUTING.md has the reasoning. For anything large, please open an issue
     first. A security issue goes to SECURITY.md, never to a public issue.

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
