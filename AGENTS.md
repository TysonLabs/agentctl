# Shipping a change: the flow for coding agents

This is the workflow in plain words. The tools (`agentflow`, `agentctl`) handle the
fiddly details; this file handles the order. Any agent can follow it. Where a repo's own
instructions (CLAUDE.md, AGENTS.md, CONTRIBUTING) differ, the repo wins.

**Always:** start anything that takes minutes (gates, reviews, deploy checks) in the
background and keep working; your harness tells you when it exits. Never wait in a
`while …; sleep` loop. Trust exit codes and the JSON result, not your reading of a log.

1. **Plan.** Read the repo's instructions and the code you will change. Check every
   fact about existing code against its source line before relying on it.
2. **Build on a branch** off the default branch, in a worktree if others share the
   checkout: `agentflow worktree new <branch>` fetches first and branches from the
   fresh default branch (`--clone-dir target` starts a build warm on macOS). For a
   throwaway probe (a red/green check, a base comparison), use
   `agentflow worktree new --scratch` and remove it with `agentflow worktree done
   <path>`. Keep the diff to what was asked.
3. **Run targeted checks while editing:** the tests and linters for what you touched,
   not the whole suite.
4. **Review with a different agent from the one that wrote the code:** if Claude wrote
   it, `agentflow codex --base main --prompt-file brief.md --write --protocol fix`
   (fix mode); if Codex or another agent wrote it, `agentflow claude` with the same
   flags. `--protocol` supplies the reviewer's rules and the output format, so the
   brief says only what changed, the contract it must keep, and where to look hardest.
   Name the targeted tests with `--test-cmd`. The JSON's `findings` lists each fix
   (null with a warning if the answer did not follow the format: then read `final.md`).
   Then audit every fix it made: accept it, revert it, or amend it, each one for a
   stated reason. For a read-only review, drop `--write` and use `--protocol review`. For a big diff, run one pass per area with
   `--path`. If you keep a lessons folder (`AGENTFLOW_LESSONS_DIR`), add
   `--lessons <the diff's topics>` so the reviewer also checks past defects. Exit 0 means `final.md` holds a real answer; anything else means there was
   no review (see the exit table in the README). **If you are the reviewer,** this step
   is done by you: review, fix what the brief asks, report, and stop. Don't start
   another review or follow the rest of this list.
5. **Run the repo's full gate once**, after the review fixes and any merge from the
   default branch. If it fails, fix it and rerun the failing part. Where the repo has a
   `[name.gate]` table, run `agentflow gate` in the background: it runs every step, logs
   each one, and keeps a receipt per step for the exact tree, so a rerun skips what already
   passed on that tree. Exit 0 means every step passed; 2 or 124 name the step and its log.
6. **Push and open a PR** whose body says what changed, how it was verified, and any
   review fix you did not take as written, with the reason.
7. **Handle every PR review thread.** If the repo uses CodeRabbit, run
   `agentflow pr wait <number>` in the background after each push that needs a review:
   exit 0 means the head is reviewed and clean, 10 lists the open threads (with ids) in
   its JSON, and `next` says what to do for any other exit. For each thread, reply first
   ("Fixed in <sha>: …" or "Keeping as-is: …"), then resolve it. Never resolve a thread
   silently.
8. **Merge only on the go-ahead the repo's policy requires.** Changes that need a human
   step (database migrations, credentials) wait for that human.
9. **Verify the deploy against reality:**
   `agentflow ship verify <service.env> --sha <merge-sha>`. Exit 0 means the service runs
   that commit; on a timeout, the JSON's `running` says what is live. Add `--contains
   --repo <checkout>` only for a forward deploy, never when verifying a rollback. Never
   report "deployed" without this check. If the service has a release channel, announce
   it from that proof: `agentflow ship announce <service.env> --verified <verify.json>
   --title ... --body-file ...` (what changed and how to test it, in plain language).
10. **Clean up:** sync any long-lived branches the repo keeps, then remove the worktree
    and the merged branches with `agentflow worktree done <branch>` (never `--force`).

**When something looks wrong in a running service,** read it, don't guess:
`agentctl endpoints <service.env>` lists what it exposes; `agentctl get <service.env>
<path>` reads it. For logs, use `agentctl logs <service.env> --q <text> --since 30m`: it
prints readable lines, so you don't need a script to parse them. To wait for an event in a
running service, run `agentctl logs <service.env> --q <text> --wait 20m` in the background
(exit 0 = it appeared, 4 = it didn't); never sleep and re-poll. agentctl is read-only, so
it is always safe to run.

**Report** what you verified and how (commands, exit codes, SHAs), what you skipped,
and anything left for a human.
