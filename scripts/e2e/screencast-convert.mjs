// Convert an approved screencast into the four animated web formats
// (apng, webp, avif, svg-wrapped webp) in docs/assets/screencasts/.
// Input may be the mp4 — or, for maximum fidelity, the durable PNG
// frames dir a screencast run leaves in data/screencast-frames/<name>
// (printed by the screencast scripts): frames feed img2webp/apng/avif
// without a lossy mp4 generation.
//
// Usage:
//   node scripts/e2e/screencast-convert.mjs <input.mp4|frames-dir>
//     [--scale 640] [--fps 2.5] [--crf 35] [--quality 80]
//     [--name base] [--out docs/assets/screencasts]

import { spawnSync } from "node:child_process";
import {
  existsSync,
  mkdirSync,
  mkdtempSync,
  readdirSync,
  readFileSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { basename, extname, join, resolve } from "node:path";
import { FPS, screencastsDir } from "./lib/screencast-demo.mjs";

const positional = [];
const opts = {};
const argv = process.argv.slice(2);
for (let i = 0; i < argv.length; i++) {
  if (argv[i].startsWith("--")) opts[argv[i].slice(2)] = argv[++i];
  else positional.push(argv[i]);
}

const src = resolve(positional[0] ?? "");
if (!positional[0] || !existsSync(src)) {
  console.error(
    "usage: screencast-convert.mjs <input.mp4|frames-dir> [--scale 640] [--fps] [--crf 35] [--quality 80] [--name] [--out]",
  );
  process.exit(2);
}
const scale = Number(opts.scale ?? 640);
const crf = Number(opts.crf ?? 35);
const quality = Number(opts.quality ?? 80);
// SVT-AV1 preset: lower = slower + smaller at the same crf. Benchmarked
// preset 4 shaves ~20% vs the encoder default for UI screencast content.
const preset = Number(opts.preset ?? 4);
const outDir = resolve(opts.out ?? screencastsDir);
const isDir = statSync(src).isDirectory();

function ffprobeFps(file) {
  const probe = spawnSync(
    "ffprobe",
    [
      "-v",
      "error",
      "-select_streams",
      "v:0",
      "-show_entries",
      "stream=r_frame_rate",
      "-of",
      "csv=p=0",
      file,
    ],
    { encoding: "utf8" },
  );
  const [num, den] = (probe.stdout || "").trim().split("/").map(Number);
  return num && den ? num / den : FPS;
}
const fps = opts.fps ? Number(opts.fps) : isDir ? FPS : ffprobeFps(src);

let name = opts.name ?? basename(src, extname(src));
if (name === "frames") name = "screencast";

// Produce one directory of scaled PNG frames regardless of input kind.
const work = mkdtempSync(join(tmpdir(), "kbm-convert-"));
const scaledDir = join(work, "scaled");
mkdirSync(scaledDir, { recursive: true });
const run = (bin, args, label) => {
  const res = spawnSync(bin, args, { encoding: "utf8" });
  if (res.status !== 0)
    throw new Error(`${label} failed: ${res.stderr || res.stdout}`);
};
if (isDir) {
  run(
    "ffmpeg",
    [
      "-hide_banner",
      "-loglevel",
      "error",
      "-y",
      "-framerate",
      String(fps),
      "-i",
      join(src, "f%04d.png"),
      "-vf",
      `scale=${scale}:-2`,
      join(scaledDir, "f%04d.png"),
    ],
    "frames rescale",
  );
} else {
  run(
    "ffmpeg",
    [
      "-hide_banner",
      "-loglevel",
      "error",
      "-y",
      "-i",
      src,
      "-vf",
      `scale=${scale}:-2`,
      join(scaledDir, "f%04d.png"),
    ],
    "frame extraction",
  );
}
const frames = readdirSync(scaledDir)
  .filter((f) => f.endsWith(".png"))
  .sort()
  .map((f) => join(scaledDir, f));
if (frames.length === 0) throw new Error(`no frames produced from ${src}`);

mkdirSync(outDir, { recursive: true });
const base = join(outDir, name);

// 1. APNG — lossless frames straight through ffmpeg's apng muxer.
run(
  "ffmpeg",
  [
    "-hide_banner",
    "-loglevel",
    "error",
    "-y",
    "-framerate",
    String(fps),
    "-i",
    join(scaledDir, "f%04d.png"),
    "-plays",
    "0",
    "-f",
    "apng",
    `${base}.apng`,
  ],
  "apng",
);

// 2. Animated WebP — img2webp (ffmpeg here has no libwebp). -d is the
//    per-frame delay in ms matching the frame rate.
run(
  "img2webp",
  [
    "-loop",
    "0",
    "-d",
    String(Math.round(1000 / fps)),
    "-lossy",
    "-q",
    String(quality),
    "-m",
    "6",
    ...frames,
    "-o",
    `${base}.webp`,
  ],
  "img2webp",
);

// 3. Animated AVIF — ffmpeg's avif muxer + SVT-AV1 sequence encode.
run(
  "ffmpeg",
  [
    "-hide_banner",
    "-loglevel",
    "error",
    "-y",
    "-framerate",
    String(fps),
    "-i",
    join(scaledDir, "f%04d.png"),
    "-c:v",
    "libsvtav1",
    "-crf",
    String(crf),
    "-preset",
    String(preset),
    "-pix_fmt",
    "yuv420p",
    `${base}.avif`,
  ],
  "avif",
);

// 4. SVG — self-contained wrapper around a raster animation (animates
//    inside <img>; needs no JS, unlike foreignObject/script players).
//    --svg-payload webp|avif picks the embedded codec: avif lands ~3x
//    smaller but leans on newer-browser AVIFS support.
const svgPayload = opts["svg-payload"] === "avif" ? "avif" : "webp";
const info = spawnSync("webpinfo", ["-summary", `${base}.webp`], {
  encoding: "utf8",
}).stdout;
const dims = info.match(/Canvas size\s*:\s*(\d+) x (\d+)/) ?? [0, 640, 400];
const b64 = readFileSync(`${base}.${svgPayload}`).toString("base64");
writeFileSync(
  `${base}.svg`,
  `<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" viewBox="0 0 ${dims[1]} ${dims[2]}" width="${dims[1]}" height="${dims[2]}" role="img" aria-label="${name} screencast">
  <image xlink:href="data:image/${svgPayload};base64,${b64}" width="${dims[1]}" height="${dims[2]}"/>
</svg>
`,
);

console.log(
  `${frames.length} frames @ ${fps}fps -> ${base}.{apng,webp,avif,svg}`,
);
