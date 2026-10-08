# Go LLM Gateway 项目导览视频

最终视频：[`renders/go-llm-gateway-intro.mp4`](renders/go-llm-gateway-intro.mp4)，1920×1080、30 fps、约 3 分 16 秒。

## 内容

9 个场景依次讲解学习路线、HTTP 代理、模型路由、SSE 生命周期、并发与背压、SQLite 后台任务、令牌桶和本地实验入口。旁白与屏幕文案以项目 `README.md`、`docs/` 教程和 `internal/` 实现为准。

## 源文件

- `BRIEF.md`、`STORYBOARD.md`、`frame.md`：制作目标、场景顺序与视觉规范。
- `script.json`：逐场旁白和画面文案。
- `compositions/`、`index.html`：HyperFrames 场景与时间线。
- `assets/voice/`：本机中文语音合成的旁白音轨。
- `snapshots/`：九个场景的时间线预览联系表。
- `tools/`：旁白生成和时间线构建脚本。

## 重新生成

在 macOS 安装 Go 不需要；生成旁白需要系统 `say`、`ffmpeg` 与 `ffprobe`，渲染需要 HyperFrames 使用的本机 Chrome 和 FFmpeg。

```sh
npm ci
node tools/generate-voice.mjs
node tools/build.mjs
npx hyperframes check
npx hyperframes render --quality delivery --output renders/go-llm-gateway-intro.mp4
```
