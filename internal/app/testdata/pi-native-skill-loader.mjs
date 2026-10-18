import fs from "node:fs/promises";
import { pathToFileURL } from "node:url";

const { cwd, agentDir, args, frozenPaths, modulePath } = JSON.parse(process.env.POSSE_PI_SKILL_TEST);
const { DefaultResourceLoader, SettingsManager } = await import(pathToFileURL(modulePath).href);
const skillPaths = [];
for (let index = 0; index < args.length; index++) {
  if (args[index] === "--skill" && args[index + 1]) skillPaths.push(args[++index]);
}

async function loadSkills(noSkills, additionalSkillPaths) {
  const loader = new DefaultResourceLoader({
    cwd,
    agentDir,
    settingsManager: SettingsManager.create(cwd, agentDir, { projectTrusted: false }),
    noSkills,
    additionalSkillPaths,
  });
  await loader.reload();
  const { skills, diagnostics } = loader.getSkills();
  const loaded = await Promise.all(skills.map(async ({ name, filePath }) => ({
    name,
    filePath,
    content: await fs.readFile(filePath, "utf8"),
  })));
  return { skills: loaded, diagnostics };
}

const baseline = await loadSkills(false, frozenPaths);
const launch = await loadSkills(args.includes("--no-skills"), skillPaths);
process.stdout.write(JSON.stringify({ baseline, launch }));
