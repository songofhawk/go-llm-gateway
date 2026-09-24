#!/usr/bin/env bash
# 文档构建工具，与 Go 网关运行无关。MMDC 可指向本地安装的 Mermaid CLI。
set -euo pipefail
cd "$(dirname "$0")/.."
DIAGRAM_CLI="${MMDC:-mmdc}"
if ! command -v "$DIAGRAM_CLI" >/dev/null 2>&1; then
  echo 'Install @mermaid-js/mermaid-cli@11.12.0 or set MMDC to its executable.' >&2
  exit 1
fi
mkdir -p docs/assets
for source in docs/diagrams/*.mmd; do
  name="$(basename "$source" .mmd)"
  if [ -n "${PUPPETEER_CONFIG:-}" ]; then
    "$DIAGRAM_CLI" -p "$PUPPETEER_CONFIG" -c docs/diagrams/config.json -i "$source" -o "docs/assets/$name.svg" -b white -w 1200
  else
    "$DIAGRAM_CLI" -c docs/diagrams/config.json -i "$source" -o "docs/assets/$name.svg" -b white -w 1200
  fi
  # 为 SVG 设置明确尺寸，避免不同 Markdown 阅读器按默认 300px 缩小图片。
  node --input-type=module - "docs/assets/$name.svg" <<'JS'
import fs from 'node:fs';
const file=process.argv[2];
let svg=fs.readFileSync(file,'utf8');
const box=svg.match(/viewBox="([^"]+)"/)[1].split(/\s+/).map(Number);
svg=svg.replace(/<svg\b[^>]*>/,tag=>tag.replace(/ width="[^"]*"/,'').replace(/ height="[^"]*"/,'').replace('>',' width="'+Math.ceil(box[2])+'" height="'+Math.ceil(box[3])+'">'));
fs.writeFileSync(file,svg);
JS
done
