# Done Signals wait for queued instructions

`posse holler done` is rejected while a Task has queued, claimed or interrupting Lead instructions, so the Rider can receive and apply them before completion. The check shares the Signal's SQLite write transaction to prevent a concurrent queue insertion from slipping past it; reporting a done Signal with an incomplete Notice would leave the Rider unable to act on the instruction. A message in `submitting` remains governed by the uncertain-delivery protocol because Herdr may already have accepted it.
