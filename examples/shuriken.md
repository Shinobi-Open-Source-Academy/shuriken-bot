<!--
Copy this file to .github/shuriken.md in a repository you want Shuriken to
review. Shuriken reads it at the pull request's head commit and includes it in
the review prompt, cached across reviews of the same repository.

Two things to understand before you edit it:

1. It can only NARROW what Shuriken reports. It cannot grant new abilities,
   change the output format, or make Shuriken approve a pull request. Shuriken
   treats this file as repository content, not as instructions from its
   operator.

2. "Do not flag" rules are the highest-value thing in here. Shuriken is told
   not to raise style issues unless they violate a documented convention, so
   this file is how you document them — and how you silence a false positive
   you keep seeing.

Keep it short. A long file crowds out the diff.
-->

# Shuriken conventions — SOS Academy

## What this project is

A pnpm/turbo monorepo. NestJS API in `apps/server`, Next.js frontends in
`apps/{website,admin,blog,hacker}`, shared code in `libs/shared`. MongoDB via
Mongoose. Most contributors here are learning open source for the first time.

## Project conventions

These are documented decisions. Flag code that violates them.

- **Mongoose schemas own their indexes.** Every `@Prop` that gets queried needs
  a corresponding `Schema.index()` call at the bottom of the schema file.
- **DTOs validate at the boundary.** Request bodies use `class-validator`
  decorators. A controller that reads an unvalidated field off the body is a
  bug, not a style preference.
- **No secrets in code.** Configuration comes from `envConfig`
  (`apps/server/src/common/config/env.config.ts`), never `process.env` read
  directly in a service.
- **Errors use Nest's HTTP exceptions.** `throw new BadRequestException(...)`,
  not a bare `throw new Error(...)` in a controller or service.
- **Shared types live in `libs/shared`.** A type duplicated between the server
  and a frontend should be imported from there instead.

## Do not flag

Tooling already owns these, or we have decided against them. Raising them costs
a contributor's trust for no benefit.

- Formatting, quote style, semicolons, line length, import order. Biome owns
  all of it and runs in CI.
- Missing semicolons or trailing commas — again, Biome.
- `any` in test files. We allow it to keep fixtures readable.
- Missing JSDoc on exported functions. We do not require it.
- Commit message or branch name format. `commitlint` and the `pr-title`
  workflow enforce those.
- Choice of test framework or assertion style.
- Suggestions to switch libraries, add a dependency, or restructure the
  monorepo. Those are maintainer decisions, not review comments.
- `console.log` in `apps/kunai-bot` — it is a CLI-style bot and that is its
  logging.

## Worth extra attention

- **Mongoose queries missing `.lean()`** on read-only paths that return many
  documents.
- **Unbounded queries.** A `find()` with no limit on a collection that grows.
- **Anything touching `githubProfile`.** It is how contributors are identified
  across the platform; a mistake there silently detaches someone's history.
- **Auth guards.** A new controller route with no guard, where its siblings
  have one, is usually an oversight.

## Teaching notes

Most pull requests here come from people making their first open source
contribution. When a finding is conceptual, prefer naming the concept and
pointing at what to read over writing the patch for them.
