# 22. Compatible dispatch choices and resource counts

New Worker dispatch may disable an agent kind with `kinds.<kind>.disabled = true`. This is a selection guard, not deletion: it does not block an existing Lead, invalidate stored Profiles, or change the frozen launch used by an admitted Task. Dispatch fails closed when an explicit Profile, configured rule/default or implicit Lead choice resolves to a disabled kind. It never substitutes another kind.

When no Dispatch Rule or default applies, `defaults.dispatch_inheritance` selects between:

- `kind` (default): inherit only the running Lead's kind, with no model or effort, preserving existing behavior.
- `lead_profile`: inherit the configured `lead.profiles.<kind>` Profile, including its model, effort and arguments. If no named Lead Profile exists, inherit only the kind.

`posse dispatch` and `posse start` show the selected Profile, configured model and effort, and whether the configured model is unambiguously present in the launch arguments. Unknown identity stays unknown; no model is inferred or downgraded. The Task stores the selected Profile and rule so historical readers and frozen recovery do not depend on current dispatch eligibility.

`pool.keep_idle = 0` is valid and means no idle Sandbox retention. Execution budgets such as `defaults.max_workers` and `defaults.recovery_attempts` remain strictly positive. `posse doctor` reports active Workers, tracked native children and parked held-Sandbox resources separately. Native child execution is not implemented, so the tracked native-child count is zero and does not claim to observe unregistered harness subprocesses. Parked resources count held Sandboxes whose Tasks are outside active execution states; they do not consume the active Worker budget.

No database migration is needed: dispatch settings affect new resolution, while admitted plans already freeze launch identity and settings for relaunch.
