---
type: ship                  # ship | scout | review
title: Fix flaky login test
done_when: login spec passes 20 consecutive runs in CI mode
landing_mode: pr            # optional, tighten only
# review_of: t12            # review only
autonomy: { land: ask }     # optional, tighten only
---
Fix the flaky login behavior and preserve the user's intent in the Brief.
