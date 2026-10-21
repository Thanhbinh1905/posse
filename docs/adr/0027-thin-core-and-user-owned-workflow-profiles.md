# 27. Thin core and User-owned workflow profiles

Posse is a dispatch-control layer: it assigns the right work to the right isolated Worker. The User's multi-stage process is an optional workflow profile, not a required product lifecycle. The User decides between stages; the Lead does not advance them automatically.

## Context

The User agreed to a thin core and an unconfigured direct path:

> "Posse chỉ nên là lớp kiểm soát việc dispatch các worker, nó đảm bảo task giao đúng việc, indivisual, lõi đảm bảo 3 stage chính: plan into tickets -> implm the tickets/category/pattern per worker -> review -> ship handled by git. Còn như 7 phase trên là các profile thêm mà riêng tôi dùng. Còn default nó có thể chỉ là gọi 1 worker pi/claude không được config gì nhờ discover/code/plan vậy thôi"
>
> "yes đồng ý, và đảm bảo tôn chỉ là người dùng handle desicion giữa các bước chứ lead không tự động (chỉ sau này có flow afk hay gì đó)"
>
> "ok, hãy chỉ tập trung workflow thôi nhé, còn skills set thế nào mình sẽ làm sau khi cái này thành hình"

Research Tasks t335 (no-mistakes study) and t337 (revised phased-workflow report) inform this decision. Their suggestions are evidence, not the contract: automatic Lead-approved transitions, default Build bundles, skill-set work first and a compatibility alias for `review = "local"` are not adopted. [ADR 0024](0024-built-in-validation-replaces-no-mistakes.md) records the selected review modes and corrected validation order.

## Decision

### Three frames, then Ship

The core has three frames. They describe the work, not mandatory separate Tasks:

1. **Plan -> tickets:** discover the problem, plan and, when useful, produce independently implementable tickets.
2. **Implement:** one Worker per ticket, category or pattern, each with its own Task contract and Sandbox. No Worker becomes a sub-Lead or dispatches other Workers.
3. **Review:** apply the selected review mode to the candidate. A visible Review stage uses this same validation, not a second review loop.

**Ship is Git delivery after the frames**, not a fourth frame: publish/PR, forge CI and merge. Deliver does not mean production rollout, deployment permissions or monitoring.

The default remains one unconfigured `pi` or `claude` Worker that discovers, plans and codes by itself. There are no required workflow stages, bundles or Gates, and no Profile setup prerequisite. Configured review, Gates and merge authority still apply; selecting the direct path cannot bypass them. `defaults.review = "none"` is the unconfigured review default.

### Three non-disableable guardrails

No workflow profile, preference, stage approval or Autonomy setting can disable:

- **Isolation:** each repository Worker has its own Sandbox and per-Task processes. Teardown never touches resources outside its Task. Existing explicitly consented in-place directory execution keeps its scoped path leases and source-preservation rules; a profile cannot select it implicitly.
- **Ship authority:** Workers never merge or force-push. Merge follows `autonomy.merge` or explicit User approval; stage approval grants no extra merge, discard or executable authority.
- **Selected review:** the admitted review mode must be satisfied. A missing reviewer, failed validation or exhausted budget is a blocker, not permission to downgrade. A scoped User choice for a different mode is a separate admission decision, not a profile bypass.

### User-decided transitions

After a configured stage, the Lead presents the result and waits for the User's explicit decision before starting the next stage. Worker completion, Report acceptance, a green check, Notice acknowledgement, `autonomy.review` and `autonomy.merge` do not authorize a stage transition. A rejected or inconclusive result stays at that stage for a User decision about revision or stopping. Skipping or reordering a stage also requires the User's decision and cannot waive a core guardrail.

Automatic validation operations inside an already approved stage, such as bounded fix instructions and same-head CI reruns, are not stage transitions. Existing `review_loop.auto_start` can schedule within an approved Review stage; it cannot enter that stage from Build. Approving Deliver does not independently authorize merge. An AFK or autonomous workflow may be designed later only as an explicit opt-in; no such flow is part of this decision.

### Optional workflow profiles

Workflow profiles live in User config (dotfiles), with per-Project overrides. A named workflow profile is replaced as a whole by its Project override so stage lists cannot accidentally concatenate. It is distinct from a Worker **Profile**, which selects harness, model, effort and arguments. Existing Dispatch Rules still select Worker Profiles.

The User's optional flow is **Discover -> Plan -> Spec -> Tickets -> Build -> Review -> Deliver**.

- Discover uses 2-4 parallel Research Workers chosen by the Lead. A cross-model check is added only on User request, not inferred from parallel discovery.
- Plan includes critique; an arena is optional, not automatic fan-out.
- Spec records the agreed contract. Tickets group implementation by category, assigning one isolated Worker per ticket/category/pattern in Build.
- Spec/tickets used as publication-bound Reports retain independent review and exact-digest User acceptance. Stage approval is not a substitute for that evidence; the direct path does not require such Reports.
- Build implements the tickets; Review applies `none`, `quick` or `full-pipeline`; Deliver uses Git delivery.

The Bug variant is **Diagnose -> Build -> Review -> Deliver**. Diagnose maps to Plan, Build to Implement, Review to Review, and Deliver to Ship.

### Profile sketch and freezing

The following TOML is a design sketch, **not configuration accepted by the current CLI**. Stage order is array order; every stage names its frame, transition gate, Task type and Worker count. `gate = "user"` is a stage decision, not an executable Gate command. `task_type = "none"` denotes Git mechanics, not a new Task type. Worker counts are dispatch bounds subject to Project capacity, not permission to exceed it.

```toml
[workflows.feature]
stages = [
  { name = "Discover", frame = "plan", gate = "user", task_type = "research", workers = "2..4", skills = "<opaque>" },
  { name = "Plan", frame = "plan", gate = "user", task_type = "research", workers = 1, skills = "<opaque>" },
  { name = "Spec", frame = "plan", gate = "user", task_type = "research", workers = 1, skills = "<opaque>" },
  { name = "Tickets", frame = "plan", gate = "user", task_type = "research", workers = 1, skills = "<opaque>" },
  { name = "Build", frame = "implement", gate = "user", task_type = "change", workers = "one-per-ticket-category-or-pattern", skills = "<opaque>" },
  { name = "Review", frame = "review", gate = "user", task_type = "review", workers = "selected-review-mode", skills = "<opaque>" },
  { name = "Deliver", frame = "ship", gate = "user", task_type = "none", workers = 0, skills = "<opaque>" },
]

[workflows.bug]
stages = [
  { name = "Diagnose", frame = "plan", gate = "user", task_type = "research", workers = 1, skills = "<opaque>" },
  { name = "Build", frame = "implement", gate = "user", task_type = "change", workers = 1, skills = "<opaque>" },
  { name = "Review", frame = "review", gate = "user", task_type = "review", workers = "selected-review-mode", skills = "<opaque>" },
  { name = "Deliver", frame = "ship", gate = "user", task_type = "none", workers = 0, skills = "<opaque>" },
]
```

Here `ship` denotes delivery outside the three frames. Review allocates no reviewer for `none`, or one independent reviewer per round for `quick`/`full-pipeline`; full-pipeline also performs its required test/docs/lint work in the private candidate lane. Selecting `none` records review as not required, not passed; the User still decides the profile's next-stage transition.

Creating a workflow run freezes the selected profile's exact content, stage order, gates, Task types, Worker bounds, source and digest in the initial Task plan. Later Tasks, stage Decisions and status use that run snapshot, not edited dotfiles; deleting or renaming the source does not affect the run. Config changes affect new runs only. A User who wants an active run to adopt an edit starts a new run; in-place migration is not supported. No profile is frozen for an unconfigured direct Task. This is an ordered recipe over existing Tasks, not a general workflow DSL, new Task hierarchy, scheduler or five-table workflow runner.

Skill fields remain opaque placeholders. Skill sets, bundle contents, registries, loaders and default skill selection are explicitly deferred until the workflow takes shape; S01-S03 (#161-#163) are not prerequisites for the direct path or this workflow decision.

## Consequences

The core owns dispatch correctness, isolation, selected evidence and ship authority. Users can keep detailed processes in dotfiles without imposing them on every Project. Stage transitions need visible User decisions even when the next action is mechanically ready. The trade-off is deliberate: unattended progression is unavailable until separately designed and opted into.

This ADR records agreed design, not shipped workflow-profile support. [Spec sections 1.1 and 25](../spec.md#11-workflow-boundary) distinguish the direct path from planned profile and full-pipeline behavior. The implementation must preserve existing admitted plans and demonstrate an unconfigured single-Worker path as well as a User-gated profile path.
