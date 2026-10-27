# Recommended skill set

Posse ships two kinds of skills.

- **Platform skills** (`posse`, `posse-setup`, `posse-down`, `posse-update`, `posse-better-each-day`) come with the runtime and always load into Posse-launched sessions.
- **Workflow skills** are the recommended set below. They load only when listed in the `[skills]` config block, so you decide what each Lead and Worker carries ([ADR 0036](adr/0036-workflow-skills-are-selected-per-role-in-config.md)).

Install Posse first (see the README), then adopt as much of the set as you want.

## Install the set

```sh
posse skills add          # pick on a terminal
posse skills add --all    # every recommended skill
posse skills add posse-tdd posse-review
posse skills add --all --project shop   # one Project instead of the global config
posse skills                            # configured and missing skills per role
```

On a terminal, `posse skills add` opens a picker grouped by role:

```
Add Posse skills to ~/.posse/config.toml
up/down move  space toggle  a all  enter save  q quit  [+] already in config

lead
> [x] posse-whats-next           Reads Posse state and gives the User one next step...
  [ ] posse-catch-up             Catches the User up in plain words: what waits on...
change
  [ ] posse-tdd                  Implements a fix or feature test-first and proves...

1 selected, 0 already in config
```

Without a terminal it needs `--all` or skill names and prints the added skills per role. It appends to your lists, keeps comments and existing entries, and never removes anything. From a Lead it needs `--user-approved "<User's words>"`. `posse setup` offers the whole set once, and `posse setup --check` and `posse doctor` report what config lacks.

## The `[skills]` block

```toml
[skills]
lead     = ["posse-whats-next", "posse-catch-up", "posse-enlighten-me", "posse-research",
            "posse-brainstorming", "posse-sharpen", "posse-spec", "posse-tickets",
            "posse-build", "posse-arena", "posse-triage", "posse-diagnose",
            "posse-technical-writing", "posse-unslop"]
change   = ["posse-tdd", "posse-deslop", "posse-no-comments",
            "posse-technical-writing", "posse-unslop", "~/my-skills/go-style"]
review   = ["posse-review", "posse-unslop"]
research = ["posse-research", "posse-technical-writing", "posse-unslop"]
```

Each entry is a bundled skill name or an absolute or `~/` path to a directory holding `SKILL.md`. A bad entry fails config load with an error naming it. A Project list replaces the global list for that role. A Task's skills are frozen when it starts, so edits affect new Tasks and Leads only. Edit the block to remove skills or add your own.

| Key | Loaded into | Purpose |
|---|---|---|
| `lead` | the Lead session | talk with the User, clarify, plan, split, supervise |
| `change` | Workers of Change Tasks, including CI and review fix turns | edit code, test, open a PR |
| `review` | Review Round Workers | review one PR per round |
| `research` | read-only Research Workers | read sources, write a Report |

## Skills

Say the plain words in the Lead pane and the matching skill starts.

| Skill | Role | Say |
|---|---|---|
| `posse-whats-next` | lead | "what should I do now?" |
| `posse-catch-up` | lead | "where are we?" |
| `posse-enlighten-me` | lead | "explain X" |
| `posse-research` | lead, research | "research X" |
| `posse-brainstorming` | lead | "develop this idea" |
| `posse-sharpen` | lead | "lock this idea" |
| `posse-spec` | lead | "write the spec" |
| `posse-arena` | lead | "compare designs for this one-way door" |
| `posse-tickets` | lead | "break it into tickets" |
| `posse-triage` | lead | "triage these issues" |
| `posse-build` | lead | "build the tickets" |
| `posse-diagnose` | lead | "this is broken" |
| `posse-tdd` | change | Workers implement test-first |
| `posse-deslop` | change | Workers clean their own diff before publishing |
| `posse-no-comments` | change | Workers drop needless comments before publishing |
| `posse-review` | review | Review Round Workers review one PR |
| `posse-technical-writing` | lead, change, research | prose in any language |
| `posse-unslop` | lead, change, review, research | removes AI tells from prose |
