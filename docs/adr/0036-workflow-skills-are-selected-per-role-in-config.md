# 36. Workflow skills are selected per role in config

Every Lead and Worker used to load all embedded skills. Workers carried Lead skills, which cost context, invited misfires and lengthened launch commands, and the User could neither choose nor add skills. [ADR 0027](0027-thin-core-and-user-owned-workflow-profiles.md) deferred skill selection until the workflow took shape. The User then asked to treat the workflow skills as a recommendation layered on the platform rather than part of it (spec #563):

> không nên là add hay remove, hãy coi bộ skills này là đề xuất của tôi thôi, người dùng cài về thì áp dụng thêm cái workfow của tôi, còn nền tảng thì đã có sẵn rồi, thì khi cài bộ skill sẽ có thêm thao tác add hết skill này vào config và tùy biến nạp luôn

Posse always loads the platform skills: `posse`, `posse-setup`, `posse-down` and `posse-update`. It loads workflow skills only from the `[skills]` config block, which holds one list per role:

| Key | Loaded into |
|---|---|
| `lead` | the Lead session |
| `change` | Workers of Change Tasks, including their CI and review fix turns |
| `review` | Review Round Workers |
| `research` | Research Workers |

An entry is a bundled skill name or an absolute or `~/` path to a directory holding `SKILL.md`; Posse freezes that `SKILL.md` under the skill's frontmatter name. A config with an unknown name, a missing or unreadable directory, or a path skill named like a bundled skill fails to load with an error naming the entry. A Project list replaces the User list for that role. Without a `[skills]` block, sessions get only the platform skills.

The lists are explicit and complete. There are no add or remove deltas over a hidden default, so the config the User reads is the set that loads. Lists are flat per role, not `[skills.<role>]` tables; per-stage selection, if needed later, belongs to workflow profiles.

A Worker's selection is frozen into its Task plan at admission and into the content-addressed launch snapshot, as Codex instructions and preferences already are. A later config edit affects new Tasks only; relaunching an admitted Task, including with another Profile, keeps its frozen skills.

Existing installs keep their behavior: the first `posse setup` of a version with this decision, which `posse update` runs, writes the full recommended block into the User config once and records that in the setup manifest. A fresh install starts with the platform skills only.

This replaces ADR 0027's deferral of skill selection. Skills in workflow profile stages remain opaque.
