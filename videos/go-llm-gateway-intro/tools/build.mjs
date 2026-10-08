import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const project = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const script = JSON.parse(await readFile(path.join(project, "script.json"), "utf8"));
const audio = JSON.parse(await readFile(path.join(project, "audio-meta.json"), "utf8"));
const audioById = new Map(audio.scenes.map((item) => [item.id, item]));

const diagrams = {
  opening: {
    nodes: [
      ["客户端", "发送一次请求", -565, 0, "mint"],
      ["Go 网关", "路由 · 生命周期", 0, 0, "violet"],
      ["上游模型", "Mock 或真实兼容服务", 565, 0, "amber"],
    ], links: [[0, 1], [1, 2]],
  },
  roadmap: {
    nodes: [
      ["Go 基础", "语法与资源所有权", -565, 0, "violet"],
      ["请求路径", "代理与模型路由", -190, 0, "mint"],
      ["并发工作", "流式与后台任务", 190, 0, "amber"],
      ["实验复盘", "压测与请求限流", 565, 0, "blue"],
    ], links: [[0, 1], [1, 2], [2, 3]],
  },
  request: {
    nodes: [
      ["curl", "localhost:8081", -565, 0, "mint"],
      ["HTTP handler", "校验 · 转发", -190, 0, "violet"],
      ["Provider", "持有上游调用", 190, 0, "blue"],
      ["Mock LLM", "固定模拟文本", 565, 0, "amber"],
    ], links: [[0, 1], [1, 2], [2, 3]],
  },
  routing: {
    nodes: [
      ["economy", "经济档", -500, -145, "mint"],
      ["balanced", "均衡档", 0, -145, "violet"],
      ["powerful", "高能力档", 500, -145, "blue"],
      ["候选组 A", "按占用比例选择", -500, 135, "mint"],
      ["候选组 B", "有限 fallback", 0, 135, "amber"],
      ["候选组 C", "相同请求能力", 500, 135, "blue"],
    ], links: [[0, 3], [1, 4], [2, 5], [3, 4], [4, 5]],
  },
  streaming: {
    nodes: [
      ["HTTP 200", "响应开始", -565, 0, "blue"],
      ["chunk 1", "及时 Flush", -190, 0, "mint"],
      ["chunk 2…", "仍占用名额", 190, 0, "violet"],
      ["[DONE]", "Body.Close 后释放", 565, 0, "amber"],
    ], links: [[0, 1], [1, 2], [2, 3]],
  },
  capacity: {
    nodes: [
      ["请求 A", "等待上游", -500, -135, "mint"],
      ["请求 B", "正在传输", 0, -135, "blue"],
      ["请求 C", "等待下一块", 500, -135, "violet"],
      ["请求 D", "没有空位 → 503", 0, 145, "amber"],
    ], links: [[0, 1], [1, 2], [3, 1]],
  },
  jobs: {
    nodes: [
      ["queued", "已写入 SQLite", -565, 0, "mint"],
      ["running", "worker 调用上游", -190, 0, "violet"],
      ["completed", "结果已持久化", 190, 0, "blue"],
      ["done", "后处理结束", 565, 0, "amber"],
    ], links: [[0, 1], [1, 2], [2, 3]],
  },
  "rate-limit": {
    nodes: [
      ["速率", "2 个新请求 / 秒", -500, -120, "mint"],
      ["突发", "桶容量 3", 0, -120, "blue"],
      ["第 4 个", "无令牌 → 429", 500, -120, "amber"],
      ["并发", "未结束工作占名额", -250, 145, "violet"],
      ["满载", "网关返回 503", 300, 145, "amber"],
    ], links: [[0, 1], [1, 2], [3, 4]],
  },
  "next-steps": {
    nodes: [
      ["启动 3 个 Mock", "端口 9090–9092", -500, -125, "mint"],
      ["运行 gateway", "localhost:8080", 0, -125, "violet"],
      ["curl 发请求", "观察 JSON / SSE", 500, -125, "blue"],
      ["继续读源码", "lessons/ → internal/", 0, 145, "amber"],
    ], links: [[0, 1], [1, 2], [2, 3]],
  },
};

const esc = (value) => String(value).replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&quot;");
const planned = [];
let cursor = 0;
for (let i = 0; i < script.scenes.length; i += 1) {
  const scene = script.scenes[i];
  const voice = audioById.get(scene.id);
  if (!voice) throw new Error(`Missing audio for ${scene.id}`);
  const hold = i === script.scenes.length - 1 ? 1.15 : 0.5;
  const duration = voice.duration + hold;
  planned.push({ ...scene, ...diagrams[scene.id], start: Number(cursor.toFixed(3)), duration: Number(duration.toFixed(3)), voice: voice.file, index: i + 1 });
  cursor += duration;
}
const totalDuration = Number(cursor.toFixed(3));

function voiceLength(scene) {
  return audioById.get(scene.id).duration.toFixed(3);
}

const style = `
  :root { --bg: #0b1020; --fg: #f3f6fc; --muted: #9aa9bf; --mint: #59ead9; --violet: #b6bcff; --amber: #ffca74; --blue: #91c5ff; }
  * { box-sizing: border-box; }
  #root { position: relative; width: 1920px; height: 1080px; overflow: hidden; padding: 82px 132px 0; color: var(--fg); background: radial-gradient(ellipse at 74% 43%, #192647 0%, var(--bg) 55%); font-family: system-ui, sans-serif; }
  #root::before { content: ""; position: absolute; inset: 0; opacity: .18; background-image: linear-gradient(rgba(162,180,216,.10) 1px, transparent 1px), linear-gradient(90deg, rgba(162,180,216,.10) 1px, transparent 1px); background-size: 64px 64px; mask-image: linear-gradient(90deg, transparent, black 20%, black 90%, transparent); }
  .scene-header { position: relative; z-index: 2; }
  .kicker { color: var(--mint); font: 600 22px/1.2 ui-monospace, monospace; letter-spacing: .12em; text-transform: uppercase; display: flex; align-items: center; gap: 13px; }
  .live-dot { width: 10px; height: 10px; display: inline-block; border-radius: 50%; background: var(--mint); box-shadow: 0 0 22px rgba(66,214,197,.7); }
  .scene-title { margin: 24px 0 27px; max-width: 1500px; font-size: 70px; line-height: 1.15; font-weight: 720; letter-spacing: -.045em; }
  .scene-summary { margin: 0; color: var(--muted); font-size: 31px; line-height: 1.35; }
  .diagram-stage { position: absolute; z-index: 1; left: 170px; top: 300px; width: 1580px; height: 500px; overflow: visible; }
  .routes { position: absolute; inset: 0; width: 1580px; height: 500px; overflow: visible; }
  .route-line { fill: none; stroke: rgba(66,214,197,.85); stroke-width: 4; stroke-linecap: round; filter: drop-shadow(0 0 8px rgba(66,214,197,.25)); }
  .stage-node { position: absolute; left: 50%; top: 50%; width: 270px; min-height: 126px; padding: 22px 25px 20px; border: 1px solid rgba(163,181,221,.24); border-radius: 18px; background: linear-gradient(145deg, rgba(27,39,65,.98), rgba(16,24,42,.98)); box-shadow: 0 18px 55px rgba(0,0,0,.28); display: flex; flex-direction: column; justify-content: center; will-change: transform, opacity; }
  .stage-node strong { margin-top: 5px; font-size: 30px; line-height: 1.2; letter-spacing: -.025em; white-space: nowrap; }
  .stage-node small { margin-top: 9px; color: var(--muted); font-size: 20px; line-height: 1.35; white-space: nowrap; }
  .node-kicker { color: var(--mint); font: 600 14px/1.1 ui-monospace, monospace; letter-spacing: .12em; }
  .tone-mint { border-color: rgba(66,214,197,.44); }
  .tone-violet { border-color: rgba(164,170,255,.48); }
  .tone-violet .node-kicker { color: var(--violet); }
  .tone-blue { border-color: rgba(123,183,255,.45); }
  .tone-blue .node-kicker { color: var(--blue); }
  .tone-amber { border-color: rgba(255,202,116,.5); }
  .tone-amber .node-kicker { color: var(--amber); }
  .stage-mark { position: absolute; right: 7px; bottom: -22px; color: rgba(200,215,241,.88); font: 600 18px/1 ui-monospace, monospace; letter-spacing: .1em; }
  .scene-footer { position: absolute; left: 132px; right: 132px; bottom: 42px; z-index: 2; }
  .voice-caption { min-height: 90px; margin: 0 0 23px; max-width: 1570px; color: #e0e7f4; font-size: 30px; line-height: 1.48; letter-spacing: .005em; }
  .progress { height: 3px; width: 100%; background: rgba(148,165,197,.18); }
  .progress-fill { display: block; width: 100%; height: 100%; transform: scaleX(0); background: linear-gradient(90deg, var(--mint), var(--blue)); }
  .footer-meta { margin-top: 17px; display: flex; justify-content: space-between; color: #b0bfd6; font: 600 15px/1 ui-monospace, monospace; letter-spacing: .15em; }
`;

const compositionFiles = planned.map((scene) => {
  const nodes = scene.nodes.map(([title, detail, x, y, color], index) => `
          <div class="stage-node tone-${color}" data-target-x="${x}" data-target-y="${y}" data-node-index="${index}">
            <span class="node-kicker">${String(index + 1).padStart(2, "0")}</span>
            <strong>${esc(title)}</strong>
            <small>${esc(detail)}</small>
          </div>`).join("");
  const links = scene.links.map(([from, to], linkIndex) => {
    const a = scene.nodes[from]; const b = scene.nodes[to];
    const x1 = 790 + a[2]; const y1 = 250 + a[3];
    const x2 = 790 + b[2]; const y2 = 250 + b[3];
    const cx = (x1 + x2) / 2; const cy = (y1 + y2) / 2 - (x1 === x2 ? 0 : 25);
    return `<path class="route-line" data-link-index="${linkIndex}" d="M ${x1} ${y1} Q ${cx} ${cy} ${x2} ${y2}" />`;
  }).join("");
  return `
<!doctype html>
<html lang="zh-CN"><head><meta charset="UTF-8"><title>${esc(scene.title)}</title></head><body>
  <template>
    <style>${style}</style>
    <div id="root" data-composition-id="${scene.id}" data-width="1920" data-height="1080" data-duration="${scene.duration}">
        <header class="scene-header">
          <div class="kicker"><span class="live-dot"></span>${esc(scene.kicker)}</div>
          <h1 class="scene-title">${esc(scene.title)}</h1>
          <p class="scene-summary">${esc(scene.summary)}</p>
        </header>
        <div class="diagram-stage">
          <svg class="routes" viewBox="0 0 1580 500" aria-hidden="true">${links}</svg>
          ${nodes}
          <div class="stage-mark">${String(scene.index).padStart(2, "0")} / ${planned.length}</div>
        </div>
        <div class="scene-footer">
          <p class="voice-caption">${esc(scene.narration)}</p>
          <div class="progress"><span class="progress-fill" style="transform-origin:left center" data-progress="${scene.duration}"></span></div>
          <div class="footer-meta"><span>GO LLM GATEWAY</span><span>${String(scene.index).padStart(2, "0")} — ${String(planned.length).padStart(2, "0")}</span></div>
        </div>
    </div>
    <script>
      const timeline = gsap.timeline({ paused: true });
      const root = document.getElementById("root");
      root.querySelectorAll(".route-line").forEach((path, i) => {
        const length = path.getTotalLength();
        path.style.strokeDasharray = String(length);
        path.style.strokeDashoffset = String(length);
        timeline.to(path, { strokeDashoffset: 0, duration: 0.58, ease: "power2.out" }, 0.50 + i * 0.13);
      });
      timeline.fromTo(root.querySelector(".scene-title"), { y: 24, opacity: 0 }, { y: 0, opacity: 1, duration: 0.58, ease: "power3.out" }, 0.15);
      timeline.fromTo(root.querySelector(".voice-caption"), { y: 13, opacity: 0 }, { y: 0, opacity: 1, duration: 0.55, ease: "power2.out" }, 0.32);
      root.querySelectorAll(".stage-node").forEach((node, i) => {
        timeline.fromTo(node,
          { xPercent: -50, yPercent: -50, x: 0, y: 0, scale: 0.72, opacity: 0 },
          { x: Number(node.dataset.targetX), y: Number(node.dataset.targetY), scale: 1, opacity: 1, duration: 1.0, ease: "power3.out" },
          0.38 + i * 0.055,
        );
      });
      timeline.fromTo(root.querySelector(".progress-fill"), { scaleX: 0 }, { scaleX: 1, duration: ${scene.duration - 0.1}, ease: "none" }, 0.05);
      window.__timelines["${scene.id}"] = timeline;
    </script>
  </template>
</body></html>
`;
});

const hosts = planned.map((scene) => `
      <div id="${scene.id}" class="clip" data-composition-id="${scene.id}" data-composition-src="compositions/${scene.id}.html" data-start="${scene.start}" data-duration="${scene.duration}" data-track-index="1" data-width="1920" data-height="1080"></div>
      <audio id="voice-${scene.id}" src="assets/voice/${scene.id}.mp3" data-start="${scene.start}" data-duration="${voiceLength(scene)}" data-track-index="10"></audio>`).join("\n");

const html = `<!doctype html>
<html lang="zh-CN">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=1920, height=1080" />
    <title>Go LLM Gateway · 项目导览</title>
    <script src="./node_modules/gsap/dist/gsap.min.js"></script>
    <style>
      * { box-sizing: border-box; }
      html, body { margin: 0; width: 1920px; height: 1080px; overflow: hidden; background: #0b1020; }
      #root { position: relative; width: 1920px; height: 1080px; overflow: hidden; }
      .clip { position: absolute; inset: 0; }
    </style>
  </head>
  <body>
    <main id="root" data-composition-id="main" data-start="0" data-width="1920" data-height="1080" data-duration="${totalDuration}">
${hosts}
    </main>
    <script>
      window.__timelines["main"] = gsap.timeline({ paused: true });
    </script>
  </body>
</html>
`;

await import("node:fs/promises").then(({ mkdir }) => mkdir(path.join(project, "compositions"), { recursive: true }));
for (const [index, scene] of planned.entries()) {
  await writeFile(path.join(project, "compositions", `${scene.id}.html`), compositionFiles[index]);
}
await writeFile(path.join(project, "index.html"), html);
await writeFile(path.join(project, "timeline.json"), `${JSON.stringify({ duration: totalDuration, scenes: planned.map(({ id, index, start, duration, voice }) => ({ id, index, start, duration, voice })) }, null, 2)}\n`);
console.log(`Built ${planned.length} scenes · ${totalDuration.toFixed(1)}s · 1920x1080`);
