#!/bin/sh
set -eu
: "${YDFS_REPO:?Set YDFS_REPO to the absolute host checkout path}"
: "${YDFS_DATA:?Set YDFS_DATA to the absolute host data path}"
for path in "$YDFS_REPO" "$YDFS_DATA"; do
    case "$path" in
        /*) ;;
        *) echo "Repository and data paths must be absolute: $path" >&2; exit 1 ;;
    esac
    if [ "$(realpath "$path")" != "$path" ]; then
        echo "Use a physical path without symlinks or trailing slash: $path" >&2
        exit 1
    fi
done
git config --global --add safe.directory "$YDFS_REPO"
exec ydfs-web --repo "$YDFS_REPO" --data "$YDFS_DATA" "$@"
