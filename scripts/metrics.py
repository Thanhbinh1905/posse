"""Posse reliability baselines from a copy of ~/.posse/posse.db.

Rerun:
  python3 -I -c "import sqlite3;s=sqlite3.connect('file:'+__import__('os').path.expanduser('~/.posse/posse.db')+'?mode=ro',uri=True);d=sqlite3.connect('posse-copy.db');s.backup(d)"
  python3 -I metrics.py posse-copy.db [project-name]
"""
import collections, json, os, re, sqlite3, statistics, sys

db = sqlite3.connect(sys.argv[1] if len(sys.argv) > 1 else "db/posse.db")
proj = sys.argv[2] if len(sys.argv) > 2 else "posse"
q = lambda s, *a: db.execute(s, a).fetchall()
P = q("select id from projects where name=?", proj)[0][0]
HOME = os.path.expanduser(f"~/.posse/projects/{proj}/tasks")

def pct(xs, p):
    xs = sorted(xs)
    if not xs:
        return None
    k = (len(xs) - 1) * p
    lo = int(k)
    hi = min(lo + 1, len(xs) - 1)
    return xs[lo] + (xs[hi] - xs[lo]) * (k - lo)

tasks = {r[0]: dict(zip(["id", "seq", "type", "state", "launches", "created", "reviews", "pr"], r))
         for r in q("select id,seq,type,state,launches,created_at,reviews_task_id,pr_url from tasks where project_id=? order by seq", P)}
trans = collections.defaultdict(list)
for tid, f, t, src, note, at in q("select task_id,from_state,to_state,source,note,at from transitions where task_id in (select id from tasks where project_id=?) order by id", P):
    trans[tid].append((f, t, src, note, at))
sys_msg_ids = {r[0] for r in q("select message_id from validation_fix_rounds")}
msgs = collections.defaultdict(lambda: collections.Counter())
for mid, tid, body in q("select m.id,m.task_id,m.body from messages m join tasks k on k.id=m.task_id where k.project_id=? and m.status in ('delivered','submitting')", P):
    kind = "posse" if mid in sys_msg_ids or body.startswith("Posse ") else "lead"
    msgs[tid][kind] += 1
dec = collections.Counter(r[0] for r in q("select task_id from decisions where project_id=? and answered_at>0", P))
keys = collections.Counter()
if q("select 1 from sqlite_master where name='interventions'"):
    keys.update(r[0] for r in q("select task_id from interventions where project_id=? and kind='keys'", P))
sig = collections.defaultdict(list)
for tid, verb, note, at in q("select task_id,verb,note,at from signals where task_id in (select id from tasks where project_id=?) order by id", P):
    sig[tid].append((verb, note, at))

def window(label, ids):
    T = [tasks[i] for i in ids]
    ch = [t for t in T if t["type"] == "change"]
    merged = [t for t in ch if any(x[1] == "merged" for x in trans[t["id"]])]
    print(f"\n== {label}: {len(T)} Tasks (seq {T[0]['seq']}..{T[-1]['seq']}), {len(ch)} Change ==")
    print(f"change merged: {len(merged)}/{len(ch)} = {len(merged)/max(len(ch),1):.0%}")
    # interventions per Task
    lead = [msgs[t['id']]['lead'] for t in T]
    posse = [msgs[t['id']]['posse'] for t in T]
    relaunch = [max(t["launches"] - 1, 0) for t in T]
    decs = [dec[t["id"]] for t in T]
    user_tr = [sum(1 for x in trans[t["id"]] if x[2] == "user") for t in T]
    rework = [sum(1 for x in trans[t["id"]] if x[0] in ("done", "merging") and x[1] == "working" and x[2] == "lead") for t in T]
    watch_reopen = [sum(1 for x in trans[t["id"]] if x[0] in ("done", "merging") and x[1] == "working" and x[2] == "watcher") for t in T]
    total = [a + b + c + d + keys[t["id"]] for a, b, c, d, t in zip(lead, relaunch, decs, user_tr, T)]
    for name, xs in [("lead sends", lead), ("posse auto-sends", posse), ("relaunches", relaunch), ("decisions answered", decs),
                     ("user transitions", user_tr), ("lead reopen done/merging->working", rework), ("watcher reopen", watch_reopen),
                     ("interventions (lead sends+relaunch+decision+user+keys)", total)]:
        print(f"  {name}: sum={sum(xs)} mean={statistics.mean(xs):.2f} median={statistics.median(xs)} p90={pct(xs,.9):.1f} tasks>0={sum(1 for x in xs if x)}")
    zero = sum(1 for t, n in zip(T, total) if n == 0 and t["type"] == "change" and t in merged)
    print(f"  merged Change Tasks with zero interventions: {zero}/{len(merged)}")
    # done count per Change Task (completion attempts)
    dones = [sum(1 for s in sig[t['id']] if s[0] == 'done') for t in ch]
    print(f"  done Signals per Change Task: mean={statistics.mean(dones):.2f} median={statistics.median(dones)} p90={pct(dones,.9):.1f} >1:{sum(1 for d in dones if d>1)}")
    # time to merge
    ttm, ttfd = [], []
    for t in merged:
        start = next((x[4] for x in trans[t["id"]] if x[1] == "working"), t["created"])
        m = next(x[4] for x in trans[t["id"]] if x[1] == "merged")
        ttm.append((m - start) / 3.6e6)
        fd = next((x[4] for x in trans[t["id"]] if x[1] == "done"), None)
        if fd:
            ttfd.append((m - fd) / 3.6e6)
    if ttm:
        print(f"  start->merged hours: median={statistics.median(ttm):.2f} p90={pct(ttm,.9):.2f} max={max(ttm):.1f}")
        print(f"  first done->merged hours: median={statistics.median(ttfd):.2f} p90={pct(ttfd,.9):.2f}")
    # reviews
    revs = [t for t in T if t["type"] == "review"]
    by_target = collections.Counter(t["reviews"] for t in revs if t["reviews"])
    reviewed = [t for t in ch if by_target[t["id"]]]
    print(f"  review Tasks: {len(revs)}; Change Tasks reviewed: {len(reviewed)}/{len(ch)}; rounds per reviewed Change: "
          f"mean={statistics.mean([by_target[t['id']] for t in reviewed]) if reviewed else 0:.2f} max={max([by_target[t['id']] for t in reviewed], default=0)}")
    # Verdicts come from the reviewer's done note; the review_rounds table only covers the built-in loop.
    p01, cr, pas, unk = 0, 0, 0, 0
    for t in revs:
        note = " ".join(s[1] for s in sig[t["id"]] if s[0] == "done")
        if re.search(r"request[- ]?changes|changes_requested|no merge|blockers? found|confirmed .*defects|defects?\b.*high", note, re.I):
            cr += 1
            if re.search(r"\bP[01]\b|blocker|high", note, re.I):
                p01 += 1
        elif re.search(r"\baccept|\bpass\b|no findings|no blocker", note, re.I):
            pas += 1
        else:
            unk += 1
    print(f"  review verdicts from done notes: changes_requested={cr} (naming P0/P1/high/blocker: {p01}) pass={pas} unclassified={unk}")
    fixed = 0
    for t in reviewed:
        rdone = [s[2] for r in revs if r["reviews"] == t["id"] for s in sig[r["id"]] if s[0] == "done"]
        if rdone and any(s[0] == "done" and s[2] > min(rdone) for s in sig[t["id"]]):
            fixed += 1
    print(f"  reviewed Change Tasks with a later done Signal after the first review (fix after review): {fixed}/{len(reviewed)}")

ids = list(tasks)
window("all Tasks", ids)
window("last 100 Tasks", ids[-100:])

def recorded_interventions():
    """Steering actions the `interventions` table and notices record since migration 00077."""
    if not q("select 1 from sqlite_master where name='interventions'"):
        print("\n== recorded interventions: table missing, database predates migration 00077 ==")
        return
    print("\n== recorded interventions ==")
    for kind, n, tasks in q("select kind,count(*),count(distinct task_id) from interventions where project_id=? group by kind order by kind", P):
        print(f"  {kind}: {n} rows on {tasks} Tasks")
    for kind in ("send", "interrupt", "relaunch"):
        for reason, n in q("select reason,count(*) from interventions where project_id=? and kind=? group by reason order by 2 desc", P, kind):
            print(f"  {kind} reason={reason or '(none)'}: {n}")
    launches = [r[0] for r in q("select at from interventions where project_id=? and kind='lead_launch' order by at", P)]
    gaps = [(b - a) / 3.6e6 for a, b in zip(launches, launches[1:])]
    if gaps:
        print(f"  lead launches: {len(launches)}, hours between launches median={statistics.median(gaps):.2f} max={max(gaps):.1f}")
    specs = q("select count(*) from notices where project_id=? and kind='spec_ready_to_deliver'", P)[0][0]
    print(f"  spec_ready_to_deliver Notices: {specs}")

recorded_interventions()
