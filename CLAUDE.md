# Project rules

- Follow `docs/DESIGN.md`. It holds the architecture, test plan, and commit plan.
  The next step is the first plan item without a matching commit in `git log`.
- Make one small commit per change, with a clear message.
- Commit tests with the code they cover. Run them in Docker, because Go is not
  installed on the host.
- Never read, create, edit, or commit `ai.md`. The project owner writes it.
- Write all project text in English: code, comments, commits, and docs.
- Reply to the owner in Persian, keeping technical terms in English.
- Label every number as measured or estimated. Never present a guess as measured.
- Work on `feat/estimation-service`. Do not push. Merge into `main` with
  `--ff-only`, without squashing, only after the owner approves.
- Do the approved steps in the prompt, one commit per step, with tests passing at
  every commit, then stop. Report usage in one line per commit. Run a mutation
  check only when the prompt asks for it.
- Technical decisions are made by the owner. If one is needed, stop and ask.

## Usage report

Read `mcp__ccd_session_mgmt__get_usage` at the start of the work for a commit and
again after the commit. Report one short line per commit, with `tokensUsed` and
the plan percentages, and label the difference as estimated:

`<hash>: before <tokens>, 5h <n>%, weekly <n>% [measured] -> after <tokens>, 5h <n>%, weekly <n>% [measured] | diff +<tokens> [estimated]`

If usage cannot be read, say so. Never present a guess as measured.
