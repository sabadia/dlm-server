# Where the skill is installed

One file, copied. Do not fork the text per agent.

| Agent | Project path |
|---|---|
| Claude Code | `.claude/skills/intentline/SKILL.md` |
| Generic, Copilot, agentskills.io | `.agents/skills/intentline/SKILL.md` |
| GitHub | `.github/skills/intentline/SKILL.md` |
| Claude Code (user) | `~/.claude/skills/intentline/SKILL.md` |

`intentline install-agent` copies this directory into the three project paths. `--claude` also copies it to `~/.claude/skills/intentline`. An agent that can only run a shell does not need the skill file: `intentline why FILE:LINE` and `intentline capture --goal "..." --pending` are enough.

Per-agent setup lives in `docs/adapters/` in the intentline repository.
