# Contributing to ctxmesh

**ctxmesh is open source (Apache-2.0), but is not accepting external contributions yet.**

Those two facts are not in tension, and it is worth being explicit about why, because the combination
is sometimes read as a contradiction.

The **licence** governs what you may do with the code: under Apache-2.0 you can read it, run it, modify
it, fork it, and build on it commercially, with an express patent grant. That is real and permanent —
nothing here takes it back.

The **contribution policy** is a separate question about what we merge into *this* repository. Right now
the answer is nothing from outside, because the project has a single author and is working toward its
first beta. Keeping authorship settled until that cut is deliberate.

## What that means in practice

- **Pull requests will be closed**, politely and without review. Please do not spend your time on one.
- **Issues are welcome** for bugs and questions. A reproducible bug report is genuinely useful and is the
  best way to help right now.
- **Forks are fine.** That is what the licence is for. If you build something on ctxmesh, we would like
  to hear about it.
- **Security reports do not go here.** See [SECURITY.md](SECURITY.md) — please report privately.

## This is time-bounded, not permanent

This policy expires at the **first beta**. That expiry was recorded as a scheduled milestone task rather
than left to memory, specifically so it could not quietly become permanent — and it did not: the
DCO-vs-CLA question is now **decided in favour of the DCO** (ADR 0134), and the workflow is documented
below. Contributions open when the beta ships.

If you want to be told when that happens, watch the repository for releases.

## When contributions open: the DCO

We use the [Developer Certificate of Origin](https://developercertificate.org/), not a CLA. There is
nothing to sign and no form to fill in — you certify each commit with a trailer:

```sh
git commit -s -m "fix(bff): ..."
```

which appends `Signed-off-by: Your Name <your@email>`. That line is you asserting you have the right to
submit the work under the project's licence. `git config user.name` and `user.email` supply the values,
and `git rebase --signoff` fixes a branch you forgot to sign.

We chose the DCO because it is what the Linux kernel, Kubernetes and the CNCF use, so it is the mechanism
a contributor to a project of this shape already expects to meet — and because a CLA buys the ability to
relicense unilaterally, which we do not plan to do and are not willing to charge every contributor an
out-of-band signing step for. The reasoning is written down in full in ADR 0134.

## Commit messages

Every commit subject must match [Conventional Commits](https://www.conventionalcommits.org/) — the
`conventional-commits` CI job checks all of them, not just the last:

```
feat(bff): connect an OpenAI-compatible provider
fix(ui): poll while an agent is still starting
```

Scopes may contain `/` but not `,` — `fix(bff/ui)` passes, `fix(bff,ui)` does not.

## If you are here to understand the codebase

The README is the entry point. The code is a Kubernetes operator plus a control plane, and the CRDs in
`api/` are the most honest description of what the platform actually models.
