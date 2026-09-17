import test from "node:test";
import assert from "node:assert/strict";
import { readFile, writeFile, mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import ts from "typescript";

const dir = await mkdtemp(join(tmpdir(), "hastekit-skills-"));
const source = await readFile(new URL("../src/skill-upload.ts", import.meta.url), "utf8");
const code = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.ES2022, target: ts.ScriptTarget.ES2022 } }).outputText;
await writeFile(join(dir, "skill-upload.mjs"), code.replaceAll('"yaml"', JSON.stringify(import.meta.resolve("yaml"))));
const { validateSkillUpload, skillUploadPath } = await import(pathToFileURL(join(dir, "skill-upload.mjs")));
await rm(dir, { recursive: true, force: true });

function file(text, path = "") {
  return { name: "SKILL.md", webkitRelativePath: path, text: async () => text };
}

test("rejects global name conflicts including quoted YAML and Windows line endings", async () => {
  await assert.rejects(validateSkillUpload([file('\uFEFF---\r\nname: "review" # comment\r\ndescription: Review\r\n---\r\nInstructions', "folder/SKILL.md")], ["review"]), /global skill/);
});

test("allows new user names and same-named user replacements", async () => {
  await validateSkillUpload([file("---\nname: personal\ndescription: Personal\n---\nInstructions")], ["review"]);
});

test("validates the bundle root rather than nested SKILL.md resources", async () => {
  await assert.rejects(validateSkillUpload([file("", "folder/nested/SKILL.md")], []), /at its root/);
  assert.equal(skillUploadPath({name: "help.md", webkitRelativePath: "folder/refs/help.md"}), "refs/help.md");
});

test("rejects malformed or missing frontmatter", async () => {
  for (const content of ["plain text", "---\nname: review", "---\nname: [\n---", "---\ndescription: Missing name\n---"]) {
    await assert.rejects(validateSkillUpload([file(content)], []));
  }
});
