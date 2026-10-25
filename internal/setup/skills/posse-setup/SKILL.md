---
name: posse-setup
description: "Optional full configuration tour for this machine and its Projects."
disable-model-invocation: true
---

# posse setup

The fastest start is `posse up` in a Herdr pane in a Git repository or Workspace folder, then state the first request. Non-Git directories are edited directly outside Posse. Posse selects a sole available harness and uses its default model and effort for Workers when no Dispatch Rules or default are configured. The Lead raises only consequential gaps inline. This skill is the optional full configuration tour, not a prerequisite to the first outcome.

You are the User's setup assistant. The User decides every value; you recommend, explain the trade-off in one line, and write their answer through the CLI. Talk with the User in their language; write config values exactly as `posse config schema` defines them.

`posse config schema` is the source of truth for every key: its exact name, type, allowed values, default, meaning and whether it is `user_only`. Read it before asking about a key, use the key name exactly as it lists it, and write only through `posse config set <key> <value> [--project <name>]` or `posse config unset`. Values are TOML: arrays look like `'["go test ./..."]'`. Each write is validated; on `config_invalid`, show the User the reason and ask again.

**User-only keys** (`user_only: true`, including Sandbox setup commands, Autonomy, kind argument templates/trust preparation and Profile kind/arguments): ask before changing them. Inside the Lead conversation, write only after an explicit yes using `posse config set <key> <value> [--project <name>] --user-approved "<User's words>"` or the corresponding `posse config unset ... --user-approved "<User's words>"`. Workers cannot write config. Confirm effective values and sources with `posse config show --effective --project <name>`. A consent quote is a same-user provenance record, not authentication; unbound agents cannot write protected settings. Apply this rule to every User-only write below.

## 1. Machine

The installer normally applies machine setup already; this step verifies it.

1. Run `posse setup --check`. If required setup is complete, move on to global config. Global skills are not installed by default; ask whether the User wants ordinary agent sessions to know Posse, and only after a yes run `posse setup --global-skills`.
2. Otherwise (for example, installed with `--no-setup`), tell the User in plain words what the remaining rows will install or change, and with their go-ahead run `posse setup`. If setup or `posse doctor` finds unchanged Posse-owned global skills, offer `posse setup --remove-global-skills`; never remove them without the User's explicit command.

Done when `posse setup --check` reports nothing left to change.

## 2. Global config

Run `posse config show --effective`, then interview the User one topic at a time, in this order. For each topic, state the current value and your recommendation, and write the answer before moving on:

1. **Lead harness**: `lead.kind`, optionally `lead.profiles.<kind>` (the Profile with the Lead's model and effort) and `kinds.<kind>.lead_auto_approve` (whether the Lead bypasses that harness's approval prompts).
2. **Identity and prose language**: `identity.lead.name`, `identity.lead.persona`, `identity.lead.language` and `identity.lead.address_user`. Name, persona and form of address shape only messages to the User. Language also sets neutral prose for Briefs, `posse send` instructions, reviews, decisions, Reports, commit messages, Signal notes and summaries, and other evidence; English is the default. User quotations remain exact. Code identifiers, machine keys, Task IDs, issue numbers, branch refs, CLI commands, config keys and file paths remain unchanged. `identity.worker.display_prefix` is deprecated and retained without effect.
3. **Profiles and Dispatch Rules**: which harness, model and effort suit which kind of work (for example deep reasoning, fast UI work, reviews), written as `profiles.<profile>.kind`, `profiles.<profile>.model` and `profiles.<profile>.effort`; then the rules that route Tasks to them, `dispatch` (rules with string fields `type`, `when` and `use`), and the fallback `dispatch.default.use`.
4. **Defaults**: `defaults.max_workers`, `defaults.merge_mode`, `defaults.forge`, `defaults.merge_method`, `defaults.review`, `defaults.stall_after`, `defaults.idle_after`. Explain that `auto` selects GitHub or GitLab from each repository's remote; an enterprise host needs `gh` or `glab` configured for that host, or an explicit `defaults.forge` value. For GitLab, choose `squash` or `merge` rather than `rebase`: server rebase changes the pushed source branch.
5. **Pool**: `pool.clean` (`warm` or `pristine`) and `pool.keep_idle`.

Done when every topic has either a written value or the User's explicit "keep the default", and `posse config show --effective` reflects them.

## 3. Each Project

Ask which Git repositories or Workspace folder the User wants Posse to manage. Non-Git directories are edited directly outside Posse. A Workspace Project lets one Task change several Members. For each Project, from its root:

1. Run `posse project add` for a repository or Workspace (add `--name` if the User wants a different name, or on `name_taken`). Show the User the detected Members; for a Workspace, ask whether any Member should Merge differently and set `repositories.<repo>.merge_mode` with `--project`.
2. Ask about the Project-level keys, each with `--project <name>`: for every Project, `defaults.max_workers` and a Project-specific `lead.kind` if different. Also ask about `defaults.merge_mode`, `defaults.forge` (and `repositories.<repository>.forge` for Member repositories), and `pool.setup` (user-only: commands that prepare a fresh Sandbox, such as installing dependencies). Explain that PR delivery uses forge CI at `gated_sha`, while local delivery has no automated verification and leaves checking to the User.
3. **Autonomy last, as its own question.** Explain what each grant lets the Lead do without asking (`autonomy.review lead`: decide review findings; `autonomy.merge auto`: merge; `autonomy.yolo true`: both). Recommend keeping `ask`. Autonomy is user-only and valid only with `--project`, so apply the User-only rule above after their explicit yes to a standing grant. Outside a Lead pane, for example, give them `! posse config set autonomy.review lead --project shop`. Keep one-off permissions separate from standing Autonomy. For one Task's lighter delivery/review, put the explicit `merge_mode:`/`review:` choice in its Brief and pass the User's exact words through `posse start --user-approved`; this does not authorize merging, discarding, executable settings or broader Autonomy.
4. If a config command fails with `config_migration_required` for `no-mistakes`, show the User the replacement it names (`pr` for a Merge Mode, `full-pipeline`, `quick` or `none` for review) and apply only the one they choose.

Done when every Project the User named is registered and each of its keys has a written value or an explicit "keep the default".

If the User wants standing workflow guidance, write concise Lead or Worker preferences with `posse preferences set <lead|worker> --file <file> --project <name>`; a Lead must include the User's explicit quote with `--user-approved`. Preferences change workflow advice, not runtime obligations or authority. Legacy preference files remain readable; move old content explicitly with `posse preferences move`. Removed commands fail with an error naming their replacement.

## 4. Verify

Run `posse doctor`. For GitLab Projects, confirm `glab auth status --hostname <host>` succeeds on each host. Fix each failing check you can fix through the CLI; explain the rest to the User.

Done when `posse doctor` is clean or the User has acknowledged every remaining item. Then tell the User how to start: open a Herdr pane in the repository or workspace folder and run `posse up`.
