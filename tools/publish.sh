#!/bin/bash
# Publishes site/ to the gh-pages branch of this repository, which GitHub Pages serves and
# CoilShift fetches the packages from (index.json + pool/).
#
#   tools/build.sh --site && tools/publish.sh
#
# PUBLIC and outward-facing: everything in site/ (the package archives, i.e. their full
# source, plus the payload images) becomes downloadable by anyone with the URL. The script
# refuses to push if tools/leakscan.sh finds anything in site/ (local.env's LEAK_PATTERN is
# required). The sources on main are pushed with plain git, after `tools/leakscan.sh --git`.
#
# PAGES_REPO=owner/name picks another repository. COMMIT_TRAILER adds lines to the commit
# message. The first run also turns GitHub Pages on (branch gh-pages, root).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
repo=${PAGES_REPO:-ivan-prodanov/coilshift-repo}
branch=gh-pages
owner=${repo%%/*} name=${repo#*/}
url="https://$owner.github.io/$name/"
site=$root/site
work=$root/.pages

[ -f "$site/index.json" ] || { echo "no site/index.json: run tools/build.sh --site first" >&2; exit 1; }
[ -f "$root/local.env" ] && . "$root/local.env"
[ -n "${LEAK_PATTERN:-}" ] || { echo "refusing to publish: no LEAK_PATTERN in local.env (see local.env.example)" >&2; exit 1; }

# Nothing that identifies the car or this machine may leave: the index and every archive and
# image in pool/, unpacked.
"$root/tools/leakscan.sh" "$site" || { echo "refusing to publish: site/ leaks (above)" >&2; exit 1; }

if [ -d "$work/.git" ]; then
    git -C "$work" fetch -q origin "$branch"
    git -C "$work" checkout -q -B "$branch" "origin/$branch"
else
    gh repo clone "$repo" "$work" -- -q --branch "$branch" --single-branch 2>/dev/null
fi

# The branch holds exactly site/, plus .nojekyll so Pages serves every file as-is.
rsync -a --delete --exclude .git --exclude .nojekyll "$site/" "$work/"
touch "$work/.nojekyll"

git -C "$work" add -A
if git -C "$work" diff --cached --quiet; then
    echo "nothing changed since the last publish"
else
    summary=$(python3 -I -c 'import json,sys; print(", ".join(p["id"].split(".")[-1] + " " + p["version"] for p in json.load(open(sys.argv[1]))["packages"]))' "$site/index.json")
    msg="Publish: $summary"
    [ -z "${COMMIT_TRAILER:-}" ] || msg=$(printf '%s\n\n%s' "$msg" "$COMMIT_TRAILER")
    git -C "$work" commit -q -m "$msg"
    git -C "$work" push -q origin "$branch"
    echo "pushed: $summary"
fi

if ! gh api "repos/$repo/pages" >/dev/null 2>&1; then
    gh api -X POST "repos/$repo/pages" -f "source[branch]=$branch" -f 'source[path]=/' >/dev/null
    echo "GitHub Pages turned on (the first deploy takes a minute or two)"
fi
echo "repository URL for CoilShift: $url"
