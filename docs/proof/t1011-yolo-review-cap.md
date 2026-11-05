# YOLO Review-cap recovery

## Commands after installation

This change requires a newly built Posse CLI and regenerated Lead instructions. Restart or restore the Lead after the reviewed release is installed. Do not use these flags with an older installed CLI.

With Project YOLO enabled, the Lead records a Review-cap choice without claiming a User answer:

```sh
posse decide <cap-id> accept-head --lead-reason "The exact candidate risk is acceptable"
posse apply <cap-id>
```

The same flag supports `final-fix`, `rethink`, and `discard`. Without YOLO, record the User's new choice with `--user-approved` instead. Review acceptance does not complete test, docs, lint, or forge checks.

`final-fix` permits one final repair and one independent Review beyond the frozen cap. When no blocking patterns remain, it grants Review without a repair. Run `posse review start <change>` after repair. One verdict consumes the allowance; an inconclusive verdict does not renew it. Retrying the same Decision returns its stored application result. A distinct Decision cannot grant another allowance. The grant and its authorizing Decision survive snapshot rebuild even after notification history ages out.

## Recovery for t984

After installation, the Lead can make a new authorized rethink choice under YOLO:

```sh
posse decide 1131 rethink --recover --lead-reason "Final-fix could not apply; replace t984's approach. Decision 1136 records the earlier recovery discussion."
```

The output names a new Decision. Apply that new ID, then start a Brief containing `supersedes: t984`:

```sh
posse apply <new-decision-id>
posse start --brief <successor-brief>
```

The new Decision records the Lead as actor and binds the current candidate. Neither Decision 1131 nor Decision 1136 is rewritten. Decision 1136 is context, not authority. Without YOLO, the User must make a new explicit choice; a Lead must not reuse the historical quote as a new answer.

No live t984 state, Project configuration, or installed binary was changed during implementation.

## Verification

The isolated CLI E2E fixture uses its own Herdr server, Git repositories, and Posse home. Tests cover audited YOLO acceptance and discard, Worker refusal, non-YOLO refusal, exact-head acceptance, changed targets, local-stage enforcement, rethink provenance, empty-pattern final-fix, and a fresh bounded final Review. Existing workspace cap and discard tests exercise Member heads and closure safety.

Red-before evidence:

- A YOLO Lead's `decide` without a User quote returned `user_only`.
- `--lead-reason` and `--recover` were unknown flags.
- Review after the final repair returned `review_round_cap` at two used rounds against a frozen cap of one.

Review-fix regression checks reproduce retries returning `review_final_fix_spent` before the fix. The CLI tests now retry both repair and empty-pattern applications, age an acknowledged Notice beyond 128 historical entries, rebuild snapshots, and verify that the existing allowance survives without renewal. A distinct Decision remains refused.

Green checks are recorded in the candidate description and validation stages.

## Risk and rollback

Migration 94 adds audit fields to Decisions and head dispositions. Migration 95 stores the one-time final Review grant and application result in `review_cap_grants`, included in Task snapshots. The notification Notice is not authority. Historical records retain User attribution; legacy snapshots default missing attribution to User. Delegated discard keeps the existing stopped-writer, exact-instance, PR validation, and closure checks, and records its Decision in the teardown intent.

Rollback: stop Posse activity, back up the Posse home, and revert this commit. Preserve the audit columns and Decision history when returning to an older binary; do not delete delegated Decisions to make an older schema appear current. Restoring a pre-migration home loses later activity and requires an explicit User recovery choice.
