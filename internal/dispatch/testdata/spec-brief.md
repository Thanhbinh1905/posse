---
type: change                  # change | research | review
title: Fix flaky login test
done_when: login spec passes 20 consecutive runs in CI mode
merge_mode: pr            # optional, tighten only
# base: staging              # optional Task start branch
# target: staging            # optional PR/MR target; workspace may use {backend: staging}
# review_of: t12            # review only
autonomy: { merge: ask }     # optional, tighten only
---
Fix the flaky login behavior and preserve the user's intent in the Brief.
