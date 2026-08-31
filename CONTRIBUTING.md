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

This policy expires at the **first beta**. At that point we will decide between a DCO and a CLA, publish
a contributor workflow, and open up. That expiry is recorded as a scheduled milestone task rather than
left to memory, specifically so it cannot quietly become permanent.

If you want to be told when that happens, watch the repository for releases.

## If you are here to understand the codebase

The README is the entry point. The code is a Kubernetes operator plus a control plane, and the CRDs in
`api/` are the most honest description of what the platform actually models.
