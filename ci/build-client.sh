#!/bin/sh
# 把客户端编成可分发的二进制包，并写好后端要吐的那份发布说明（latest.json）。
# 使用者不需要装 Go；只有发布机需要。产物默认落在仓库里的 .dist/（已 git-ignore），挂上版本 tag 后由 Actions 发到 Release。
set -eu
VERSION=${VERSION:-$(git rev-parse --short HEAD | sed 's/^/v0.0-/')}
DIST_DIR=${DIST_DIR:-.dist}
TARGETS='linux-amd64 darwin-arm64'
SKILL=.agents/skills/emporetic-agent
EXAMPLES=examples
STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

digest() { # sha256 <file> → 十六进制摘要
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

for target in $TARGETS; do
  os=${target%%-*}; arch=${target##*-}
  out=$STAGE/$VERSION/emporetic-agent_${os}-${arch}
  mkdir -p "$out/versions/$VERSION"
  bundle=$out/versions/$VERSION
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X emporetic/agent-client/internal/client.Version=$VERSION" -o "$bundle/emporetic-agent" ./cmd/emporetic-agent
  # 二进制之外真正值钱的是那本技能：编程 Agent 读它才知道怎么接单、开工、交付与拆单。
  [ -d "$SKILL" ] || { echo "missing $SKILL, refuse to publish a package without the skill" >&2; exit 1; }
  rm -rf "$bundle/.agents"; mkdir -p "$bundle/.agents/skills"
  cp -R "$SKILL" "$bundle/.agents/skills/"
  # 发行包也能直接配置 Pi：共享参数解析器与调用说明必须和适配器一起交付。
  for required in README.md adapter.mjs adapter-args.mjs adapter-runtime.mjs github-publisher.mjs github-check.mjs pi-executor.mjs pi-picker.mjs pi-judge.mjs; do
    [ -f "$EXAMPLES/$required" ] || { echo "missing $EXAMPLES/$required, refuse to publish incomplete adapters" >&2; exit 1; }
  done
  mkdir -p "$bundle/examples"
  cp "$EXAMPLES/README.md" "$bundle/examples/"
  for example in "$EXAMPLES/"*.mjs; do
    case "$example" in *.test.mjs) continue ;; esac
    cp "$example" "$bundle/examples/"
  done
  printf '%s\n' "Emporetic Agent Client ${VERSION} (${os}/${arch})：解包后 chmod +x，运行时依赖 Git。给编程 Agent 的技能在 .agents/skills/emporetic-agent/SKILL.md——先让那个 Agent 加载它，再谈接单做任务。Pi 与通用适配器位于 examples/，参数与配置说明见 examples/README.md；使用它们还需要 Node.js 和对应的 Agent。" > "$out/README.md"
  ln -s "versions/$VERSION" "$out/current"
  ln -s current/examples "$out/examples"
  ln -s current/.agents "$out/.agents"
  cat > "$out/emporetic-agent" <<'LAUNCHER'
#!/bin/sh
set -eu
# Resolve an invocation through PATH or a symlink to the installation entrypoint.
entry=$0
while [ -L "$entry" ]; do
  base=$(CDPATH= cd -- "$(dirname -- "$entry")" && pwd)
  target=$(readlink "$entry")
  case "$target" in /*) entry=$target ;; *) entry=$base/$target ;; esac
done
root=$(CDPATH= cd -- "$(dirname -- "$entry")" && pwd)
exec "$root/current/emporetic-agent" "$@"
LAUNCHER
  chmod +x "$out/emporetic-agent"
  tar -C "$out" -czf "$STAGE/$VERSION/emporetic-agent_${os}-${arch}.tar.gz" emporetic-agent README.md .agents examples current versions
done

mkdir -p "$DIST_DIR/$VERSION"
cp "$STAGE/$VERSION/"*.tar.gz "$DIST_DIR/$VERSION/"
python3 - "$DIST_DIR" "$VERSION" <<'PY'
import hashlib, json, os, re, sys
dist, version = sys.argv[1], sys.argv[2]
assets = []
for name in sorted(os.listdir(f'{dist}/{version}')):
    m = re.match(r'^emporetic-agent_([a-z]+)-([a-z0-9]+)\.tar\.gz$', name)
    if not m:
        continue
    path = f'{dist}/{version}/{name}'
    assets.append({'name': name, 'os': m.group(1), 'arch': m.group(2),
                   'sha256': hashlib.sha256(open(path, 'rb').read()).hexdigest(),
                   'size_bytes': os.path.getsize(path)})
with open(f'{dist}/latest.json', 'w') as fp:
    json.dump({'version': version, 'assets': assets}, fp, indent=1)
print(f'published {version}: {len(assets)} packages -> {dist}')
PY
