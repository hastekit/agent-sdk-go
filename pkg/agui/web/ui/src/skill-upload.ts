import { parse } from "yaml";

export function skillUploadPath(file: Pick<File, "name" | "webkitRelativePath">): string {
  return file.webkitRelativePath ? file.webkitRelativePath.split("/").slice(1).join("/") : file.name;
}

// This is upload feedback only. The server independently validates bundles and
// gives global skills precedence even when clients bypass this check.
export async function validateSkillUpload(files: File[], globalNames: string[]): Promise<void> {
  const roots = files.filter(file => skillUploadPath(file) === "SKILL.md");
  if (roots.length !== 1) throw new Error("Choose one skill folder with SKILL.md at its root.");
  const lines = (await roots[0].text()).replace(/^\uFEFF/, "").replace(/\r\n/g, "\n").split("\n");
  const end = lines.findIndex((line, index) => index > 0 && line.trim() === "---");
  if (lines[0].trim() !== "---" || end < 0) throw new Error("SKILL.md must include YAML frontmatter with a name and description.");
  const metadata = parse(lines.slice(1, end).join("\n"));
  if (typeof metadata?.name !== "string" || !metadata.name.trim()) throw new Error("SKILL.md must include a skill name.");
  if (globalNames.includes(metadata.name)) {
    throw new Error(`“${metadata.name}” is a global skill. Change the name in SKILL.md before uploading.`);
  }
}
