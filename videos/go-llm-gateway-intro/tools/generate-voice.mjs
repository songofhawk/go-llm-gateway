import { spawnSync } from "node:child_process";
import { mkdir, readFile, rm } from "node:fs/promises";
import path from "node:path";
import os from "node:os";
import { fileURLToPath } from "node:url";

const project = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const script = JSON.parse(await readFile(path.join(project, "script.json"), "utf8"));
const output = path.join(project, "assets", "voice");
await mkdir(output, { recursive: true });

function run(command, args) {
  const result = spawnSync(command, args, { encoding: "utf8" });
  if (result.status !== 0) {
    throw new Error(`${command} failed: ${result.stderr || result.error || result.status}`);
  }
  return result.stdout.trim();
}

const metadata = [];
for (const scene of script.scenes) {
  const raw = path.join(os.tmpdir(), `go-llm-gateway-${scene.id}.aiff`);
  const audio = path.join(output, `${scene.id}.mp3`);
  try {
    run("say", ["-v", script.voice, "-r", String(script.rate), "-o", raw, "--", scene.narration]);
    run("ffmpeg", ["-y", "-hide_banner", "-loglevel", "error", "-i", raw, "-ar", "48000", "-ac", "1", "-b:a", "128k", audio]);
    const duration = Number(run("ffprobe", ["-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", audio]));
    metadata.push({ id: scene.id, file: `assets/voice/${scene.id}.mp3`, duration: Number(duration.toFixed(3)) });
    console.log(`${scene.id}: ${duration.toFixed(2)}s`);
  } finally {
    await rm(raw, { force: true });
  }
}

await import("node:fs/promises").then(({ writeFile }) => writeFile(
  path.join(project, "audio-meta.json"),
  `${JSON.stringify({ voice: script.voice, rate: script.rate, scenes: metadata }, null, 2)}\n`,
));
console.log(`Wrote ${metadata.length} voice lines to ${path.relative(project, output)}`);
