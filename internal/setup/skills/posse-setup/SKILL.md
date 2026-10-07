---
name: posse-setup
description: "Optional full configuration tour for this machine and its Projects."
disable-model-invocation: true
---

# posse setup

The fastest start is `posse up` in a Herdr pane in the repository or workspace folder, then state the first request. Posse selects a sole available harness and uses its default model and effort for Riders when no Dispatch Rules or default are configured. The Lead raises only consequential gaps inline. This skill is the optional full configuration tour, not a prerequisite to the first outcome.

You are the User's setup assistant. The User decides every value; you recommend, explain the trade-off in one line, and write their answer through the CLI. Talk with the User in their language; write config values exactly as `posse config schema` defines them.

`posse config schema` is the source of truth for every key: its exact name, type, allowed values, default, meaning and whether it is `user_only`. Read it before asking about a key, use the key name exactly as it lists it, and write only through `posse config set <key> <value> [--project <name>]` or `posse config unset`. Values are TOML: arrays look like `'["go test ./..."]'`. Each write is validated; on `config_invalid`, show the User the reason and ask again.

**User-only keys** (`user_only: true`, such as `defaults.gate`, `repositories.<member>.gate`, `remuda.setup`, `kinds.<kind>.lead_auto_approve` and `autonomy.*`): ask before changing them. Outside a Lead pane, give the User the exact command to run themselves with the `! ` prefix, for example `! posse config set defaults.gate '["go test ./..."]' --project shop`. Inside the Lead conversation, write only after an explicit yes using `posse config set <key> <value> [--project <name>] --user-approved "<User's words>"` or the corresponding `posse config unset ... --user-approved "<User's words>"`. Riders cannot write config. Confirm with `posse config show --project <name>`. Apply this rule to every User-only write below.

## 1. Machine

The installer normally applies machine setup already; this step verifies it.

1. Run `posse setup --check`. If required setup is complete, move on to global config. Global skills are not installed by default; ask whether the User wants ordinary agent sessions to know Posse, and only after a yes run `posse setup --global-skills`.
2. Otherwise (for example, installed with `--no-setup`), tell the User in plain words what the remaining rows will install or change, and with their go-ahead run `posse setup`. If setup or `posse doctor` finds unchanged Posse-owned global skills, offer `posse setup --remove-global-skills`; never remove them without the User's explicit command.

Done when `posse setup --check` reports nothing left to change.

## 2. Global config

Run `posse config show --effective`, then interview the User one topic at a time, in this order. For each topic, state the current value and your recommendation, and write the answer before moving on:

1. **Lead harness**: `lead.kind`, optionally `lead.profiles.<kind>` (the Profile with the Lead's model and effort) and `kinds.<kind>.lead_auto_approve` (whether the Lead bypasses that harness's approval prompts).
2. **Identity**: `identity.lead.name`, `identity.lead.persona`, `identity.lead.language` and `identity.lead.address_user`. Identity shapes only how the Lead talks to the User; Workers never see it. The legacy `identity.worker.display_prefix` setting remains accepted but does not affect Herdr workspace labels.
3. **Profiles and Dispatch Rules**: which harness, model and effort suit which kind of work (for example deep reasoning, fast UI work, reviews), written as `profiles.<profile>.kind`, `profiles.<profile>.model` and `profiles.<profile>.effort`; then the rules that route Tasks to them, `dispatch` (rules with string fields `type`, `when` and `use`), and the fallback `dispatch.default.use`.
4. **Defaults**: `defaults.max_workers`, `defaults.landing_mode`, `defaults.forge`, `defaults.merge_method`, `defaults.review`, `defaults.stall_after`, `defaults.idle_after`. Explain that `auto` selects GitHub or GitLab from each repository's remote; an enterprise host needs `gh` or `glab` configured for that host, or an explicit `defaults.forge` value. For GitLab, choose `squash` or `merge` rather than `rebase`: server rebase changes the pushed source branch.
5. **Remuda**: `remuda.clean` (`warm` or `pristine`) and `remuda.keep_idle`.

Done when every topic has either a written value or the User's explicit "keep the default", and `posse config show --effective` reflects them.

## 3. Each Project

Ask which repositories the User wants posse in. A folder that holds several repositories of one stack can be one workspace Project: its Lead sees every member and one Task can change several. For each Project, from its root (the repository, or the workspace folder):

1. Run `posse project add` (add `--name` if the User wants a different name, or on `name_taken`). Show the User the members it detected; for a workspace, ask whether any member should Land differently and set `repositories.<repo>.landing_mode` with `--project`.
2. Ask about the Project-level keys, each with `--project <name>`: `defaults.landing_mode`, `defaults.forge` (and `repositories.<repository>.forge` for member repositories), `defaults.gate` (user-only: commands that must pass before landing), `remuda.setup` (user-only: commands that prepare a fresh Mount, such as installing dependencies), `defaults.max_workers`, and a Project-specific `lead.kind` if different.
3. **Autonomy last, as its own question.** Explain what each grant lets the Lead do without asking (`autonomy.review lead`: decide review findings; `autonomy.land auto`: merge; `autonomy.yolo true`: both). Recommend keeping `ask`. Autonomy is user-only and valid only with `--project`, so apply the User-only rule above after their explicit yes to a standing grant. Outside a Lead pane, for example, give them `! posse config set autonomy.review lead --project shop`. Keep one-off permissions separate from standing Autonomy.
4. If `defaults.landing_mode` is `no-mistakes` and the repository is not initialized, tell the User to run `no-mistakes init` there.

Done when every Project the User named is registered and each of its keys has a written value or an explicit "keep the default".

If the User wants standing workflow guidance, write concise Lead or Rider preferences with `posse preferences set <lead|rider> --file <file> --project <name>`; a Lead must include the User's explicit quote with `--user-approved`. Preferences change workflow advice, not runtime obligations or authority. Legacy `playbook/` paths and `posse playbook` commands remain readable with deprecation warnings; move old content explicitly with `posse preferences move`.

## 4. Verify

Run `posse doctor`. For GitLab Projects, confirm `glab auth status --hostname <host>` succeeds on each host. Fix each failing check you can fix through the CLI; explain the rest to the User.

Done when `posse doctor` is clean or the User has acknowledged every remaining item. Then tell the User how to start: open a Herdr pane in the repository or workspace folder and run `posse up`.
