# Tencent Gongfeng

The first integration targets the official service at `https://git.code.tencent.com`.
Git checkout and push use the runtime host's Git credentials. MR commands use
[gongfeng-cli](https://github.com/studyzy/gongfeng-cli), a community CLI; consult
`gongfeng --help` and the relevant subcommand's `--help` before use.

## Authentication and reads

The runtime operator installs and authenticates the CLI. It accepts
`GONGFENG_TOKEN` or its configuration file; never write a token into repository
files or task comments. The workspace's encrypted integration token is only
used by the server and is not delivered to the runtime. Missing CLI or account
access is a prerequisite to report, not a reason to claim success.

```sh
gongfeng --project-id <namespace/project> mr list --json
gongfeng --project-id <namespace/project> mr show <global-mr-id> --json
```

The CLI's `show`, `update`, and `accept` commands use the global MR `id` returned
by the API. Multica's card number and the repository MR URL use `iid`. Read the
list response to resolve the global id; do not pass the card number as an id.

## Creating an MR

Push the task branch using Git, then create the MR in that repository:

```sh
gongfeng --project-id <namespace/project> mr create \
  --source-branch <task-branch> \
  --target-branch <target-branch> \
  --title "<issue-identifier> Short title" \
  --description "Closes <issue-identifier>"
```

Resolve the target branch from the project's starting branch or an existing MR.
A tag or commit is only a starting point; confirm the target branch. Keep an
existing MR's target on resumed work. Include a title/branch issue identifier
or a closing body reference to link the MR; bare body mentions do not link.
Read `issues.md` for the shared merge/status contract. Multica does not create
MRs or merge code on the agent's behalf. The server refreshes CI status for
linked MRs; runtime CLI credentials remain separate from the server token.

For commit-check writes, the official API accepts `pending`, `success`, `error`,
and `failure`. The pinned community CLI's help lists other state names; use the
official values with `gongfeng commit-status create --state`.
