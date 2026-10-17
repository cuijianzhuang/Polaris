#!/usr/bin/env bash
# Puts the closed-source polaris-pro files into this tree so that
# `go build -tags pro` builds the paid edition. The files keep their paths
# (internal/panel/xxx_pro.go ...) and are ignored by git here.
#
#   scripts/pro.sh link  [DIR]   symlink (development: edits land in DIR)
#   scripts/pro.sh copy  [DIR]   copy (CI and release builds)
#   scripts/pro.sh clean         remove them again
#
# DIR defaults to ../polaris-pro.
set -euo pipefail
cd "$(dirname "$0")/.."
mode=${1:-}
dir=${2:-../polaris-pro}

clean() {
  # only files that git ignores as pro sources
  find internal cmd -name '*_pro.go' -o -name '*_pro_test.go' 2>/dev/null | while read -r f; do
    if git check-ignore -q "$f"; then rm -f "$f"; fi
  done
}

case "$mode" in
  link|copy)
    [ -d "$dir/internal" ] || { echo "polaris-pro not found at $dir" >&2; exit 1; }
    clean
    src=$(cd "$dir" && pwd)
    n=0
    while IFS= read -r f; do
      rel=${f#"$src"/}
      case "$rel" in *_pro.go|*_pro_test.go) ;; *) continue ;; esac
      mkdir -p "$(dirname "$rel")"
      if [ "$mode" = link ]; then ln -sf "$f" "$rel"; else cp "$f" "$rel"; fi
      n=$((n + 1))
    done < <(find "$src/internal" "$src/cmd" -name '*.go' 2>/dev/null)
    echo "polaris-pro: $mode $n files; build with: go build -tags pro ./..."
    ;;
  clean)
    clean
    echo "polaris-pro files removed"
    ;;
  *)
    sed -n '2,10p' "$0" >&2
    exit 2
    ;;
esac
