---
name: buildgate-tdd
description: Test-driven development for buildgate build rounds. Use when the ticket adds behaviour or fixes a bug and requires tests - one seam, one failing test, minimal code, repeat.
license: MIT
---

# Buildgate TDD

Adapted from https://github.com/mattpocock/skills for an unattended, single-agent build round.

One **seam** (a public interface whose behaviour you can observe, never internals) at a time:

1. **Name the seam.** Write down which public interface you are testing before writing anything. Prefer the seams the ticket's acceptance criteria name.
2. **Red.** Write one failing test at that seam (if the ticket lists `Allowed-Files`, in one of those files). Run it and see it fail for the expected reason.
3. **Green.** Write only enough code to pass it.
4. **Repeat:** one seam, one test, one minimal implementation per cycle. Never write a batch of tests before any implementation.
5. Refactor in a separate pass after green, only inside the files the ticket allows.

A good test exercises the public interface, and its expected value comes from an independent source (the ticket's or spec's example, a known-good literal), never recomputed the way the code computes it. Never edit the read-only reference or oracle tests a round mounts; they are the external check.
