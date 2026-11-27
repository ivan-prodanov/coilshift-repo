#!/bin/bash
# Checks and builds every CoilShift package in this repo with the pinned csdk.
#
#   tools/build.sh            lint, harness-test and rule-test every package, then build
#                             dist/public/*.csp and dist/private/*.csp
#   tools/build.sh --record   the same, but rewrite rules-digests.tsv from this run instead
#                             of checking against it (after a deliberate rule-set change):
#                             every rule set on the primary build, and on the other builds
#                             only the rows that are already in the table
#   tools/build.sh --site     also write site/ (index.json + pool/) from dist/public/ with
#                             `csdk repo build`, ready to upload to a public HTTPS host
#
# FW_LIBS="build=/path/libQtCarGUI.so.1.0.0 ..." lists the firmware builds every rule set
# is tested against (default: the 2026.8.6 and 2026.38.6 dumps in FW_DUMP, from local.env,
# or ../tesla-dump next to this repo). The
# first is the primary build (the car's): every rule set must have a recorded digest there.
# On the others a rule set is supported only once a row for it is in rules-digests.tsv
# (added by hand after the build's gates were checked); without one its result is shown,
# not enforced. A missing library for a non-primary build is skipped with a note.
#
# A package lives in <feature>/coilshift/<package id>/; a manifest payload `payload/X` is
# read from <feature>/payloads/X (payloads are fetched by hash, never put in the archive).
# A feature with an executable <feature>/harness-seed.sh is tested on seeded fake roots
# instead of a plain `csdk test`: `harness-seed.sh --list` prints "<kind> pass|unsupported"
# lines, and for each kind both sequences run on a fresh root `harness-seed.sh ROOT KIND`
# built. `pass` must pass; `unsupported` must exit 10 on every apply and leave the root
# byte-identical to the seed; `exit-N` (a refusal) must exit N on every apply and leave the
# root byte-identical to the seed. A seed that answers `--image-tree` with "yes" gets each
# squashfs payload's files as the image's content (`csdk-harness -image-tree`), so its hooks
# can check what they mount.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
csdk="$root/tools/bin/csdk"
harness="$root/tools/bin/csdk-harness"
[ -x "$csdk" ] && [ -x "$harness" ] || "$root/tools/get-csdk.sh"
[ -f "$root/local.env" ] && . "$root/local.env"
dump=${FW_DUMP:-$root/../tesla-dump}
FW_LIBS=${FW_LIBS:-"2026.8.6-amd=$dump/2026.8.6/usr/tesla/UI/lib/libQtCarGUI.so.1.0.0 2026.38.6=$dump/2026.38.6/usr/tesla/UI/lib/libQtCarGUI.so.1.0.0"}
digests="$root/rules-digests.tsv"

# Packages whose csdk lint warnings are known and accepted (csdk has no way to silence one),
# as " <name> <name> ". direct-link and direct-summon run today's runit and egress lock under
# CoilShift (direct-link/DESIGN.md): raw sv, runsvdir, iptables/ip6tables, and kill only by a
# checked pid, all disclosed in their summaries. Every other package passes `--strict`.
allow_warnings=" direct-link direct-summon "
# Packages built into dist/private/ and never put in site/, as " <name> <name> ". None today.
# (netflix-cdm is public since 2026-11-20 by the owner's decision, after CoilShift said yes;
# its payload is Google's Widevine CDM, so that copy is Google's licence question, not ours.)
private=" "

record=0 site=0
for a in "$@"; do
    case "$a" in
        --record) record=1 ;;
        --site) site=1 ;;
        *) echo "usage: $0 [--record] [--site]" >&2; exit 2 ;;
    esac
done

fw_names=() fw_libs=() fw_shas=()
for pair in $FW_LIBS; do
    name=${pair%%=*} lib=${pair#*=}
    if [ ! -f "$lib" ]; then
        [ ${#fw_names[@]} -eq 0 ] && { echo "primary libQtCarGUI ($name) not found at $lib (set FW_LIBS)" >&2; exit 1; }
        echo "note: skipping $name, no libQtCarGUI at $lib"; continue
    fi
    fw_names+=("$name") fw_libs+=("$lib") fw_shas+=("$(shasum -a 256 "$lib" | cut -d' ' -f1)")
done

# recorded RULESET BUILD SHA: the recorded result, empty when there is no row
recorded() { awk -F'\t' -v r="$1" -v b="$2" -v s="$3" '$1==b && $2==s && $3==r {print $4}' "$digests" 2>/dev/null; }

# manifest FILE FIELD: id | version | rules (one per line) | payloads (one path per line)
manifest() {
    python3 -I - "$1" "$2" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
f = sys.argv[2]
if f in ("id", "version"):
    print(m[f])
elif f == "rules":
    print("\n".join(m.get("byte_rules", [])))
elif f == "payloads":
    print("\n".join(p["path"] for p in m.get("payloads", [])))
PY
}

failures=0
fail() { printf 'FAIL: %s\n' "$*" >&2; failures=$((failures + 1)); }

rm -rf "$root/dist"
mkdir -p "$root/dist/public" "$root/dist/private"
new_digests=$(mktemp "${TMPDIR:-/tmp}/digests.XXXXXX")
trap 'rm -f "$new_digests"' EXIT
printf 'build\tlib_sha256\trule_set\tresult\n' > "$new_digests"

for m in "$root"/*/coilshift/*/coilshift.json; do
    tree=$(dirname "$m")
    feature=$(cd "$tree/../.." && pwd)
    id=$(manifest "$m" id)
    version=$(manifest "$m" version)
    short=${id##*.}
    echo "== $id $version"

    # lint
    if [[ $allow_warnings == *" $short "* ]]; then
        "$csdk" lint "$tree" || fail "$id: lint"
    else
        "$csdk" lint --strict "$tree" || fail "$id: lint --strict"
    fi

    # harness: apply,apply and apply,remove
    payload_args=()
    while IFS= read -r p; do
        [ -n "$p" ] || continue
        f="$feature/payloads/$(basename "$p")"
        [ -f "$f" ] || { fail "$id: payload $p not found at $f"; continue; }
        payload_args+=(--payload "$p=$f")
    done < <(manifest "$m" payloads)
    seed="$feature/harness-seed.sh"
    if [ -x "$seed" ]; then
        hpay=()
        for ((k = 0; k < ${#payload_args[@]}; k += 2)); do hpay+=(-payload "${payload_args[k+1]}"); done
        trees=
        if [ "$("$seed" --image-tree 2>/dev/null || true)" = yes ]; then
            trees=$(mktemp -d "${TMPDIR:-/tmp}/csdk-tree.XXXXXX")
            for ((k = 1; k < ${#payload_args[@]}; k += 2)); do
                pp=${payload_args[k]%%=*} pf=${payload_args[k]#*=}
                unsquashfs -q -n -d "$trees/$(basename "$pf")" "$pf" >/dev/null
                hpay+=(-image-tree "$(basename "$pp")=$trees/$(basename "$pf")")
            done
        fi
        while read -r kind expect; do
            for seq in apply,apply apply,remove; do
                w=$(mktemp -d "${TMPDIR:-/tmp}/csdk-seed.XXXXXX")
                "$seed" "$w/root" "$kind" && cp -R "$w/root" "$w/seed"
                out=$("$harness" -package "$tree" -root "$w/root" -sequence "$seq" ${hpay[@]+"${hpay[@]}"} 2>&1) && rc=0 || rc=$?
                case "$expect" in
                    pass)
                        if [ "$rc" = 0 ]; then echo "ok   $id $seq ($kind root)"
                        else printf '%s\n' "$out" | sed 's/^/   /'; fail "$id: $seq on the $kind root"; fi ;;
                    unsupported)
                        applies=$(printf '%s\n' "$out" | grep -c '^  apply:' || true)
                        exit10=$(printf '%s\n' "$out" | grep -c '^  apply:.*: exit 10 ' || true)
                        if [ "$applies" -gt 0 ] && [ "$applies" = "$exit10" ] && diff -r "$w/seed" "$w/root" >/dev/null; then
                            echo "ok   $id $seq ($kind root: unsupported, nothing changed)"
                        else printf '%s\n' "$out" | sed 's/^/   /'; fail "$id: $seq on the $kind root should exit 10 and change nothing"; fi ;;
                    exit-[0-9]*)
                        n=${expect#exit-}
                        applies=$(printf '%s\n' "$out" | grep -c '^  apply:' || true)
                        exitn=$(printf '%s\n' "$out" | grep -c "^  apply:.*: exit $n " || true)
                        if [ "$applies" -gt 0 ] && [ "$applies" = "$exitn" ] && diff -r "$w/seed" "$w/root" >/dev/null; then
                            echo "ok   $id $seq ($kind root: exit $n, nothing changed)"
                        else printf '%s\n' "$out" | sed 's/^/   /'; fail "$id: $seq on the $kind root should exit $n and change nothing"; fi ;;
                    *) fail "$id: harness-seed.sh lists unknown outcome '$expect'" ;;
                esac
                rm -rf "$w"
            done
        done < <("$seed" --list)
        [ -z "$trees" ] || rm -rf "$trees"
    else
        "$csdk" test "$tree" ${payload_args[@]+"${payload_args[@]}"} || fail "$id: csdk test"
    fi

    # rule sets against each firmware build's libQtCarGUI
    while IFS= read -r r; do
        [ -n "$r" ] || continue
        rel=${tree#"$root"/}/$r
        for i in "${!fw_names[@]}"; do
            fw=${fw_names[$i]} lib=${fw_libs[$i]} sha=${fw_shas[$i]}
            out=$("$csdk" rules test "$tree/$r" "$lib" 2>&1) && result=$(printf '%s\n' "$out" | sed -n 's/.*digest \([0-9a-f]*\).*/\1/p' | head -n 1) || result=refused
            want=$(recorded "$rel" "$fw" "$sha")
            if [ "$i" -gt 0 ] && [ -z "$want" ]; then
                echo "   $fw: $result (no row in rules-digests.tsv: not supported on this build yet)"
                continue
            fi
            printf '%s\n' "$out" | sed "s/^  /     /; s|^[^ ].*libQtCarGUI.so.1.0.0:|   $fw:|"
            printf '%s\t%s\t%s\t%s\n' "$fw" "$sha" "$rel" "$result" >> "$new_digests"
            [ "$record" = 1 ] && continue
            if [ -z "$want" ]; then
                fail "$rel: no recorded digest for $fw ($result); run with --record if this is expected"
            elif [ "$want" != "$result" ]; then
                fail "$rel: $fw digest $result, recorded $want: the change list moved"
            fi
        done
    done < <(manifest "$m" rules)

    # archive, plus its payloads named by sha256 where `csdk repo build` looks for them
    dest=public
    [[ $private == *" $short "* ]] && dest=private
    "$csdk" build "$tree" -o "$root/dist/$dest/$id-$version.csp" | sed 's/^/   /'
    for ((k = 1; k < ${#payload_args[@]}; k += 2)); do
        f=${payload_args[k]#*=}
        mkdir -p "$root/dist/$dest/payloads"
        cp "$f" "$root/dist/$dest/payloads/$(shasum -a 256 "$f" | cut -d' ' -f1)"
    done
done

if [ "$record" = 1 ]; then
    # keep rows of builds/libraries not tested in this run, replace the rows of those that were
    tested=$(for i in "${!fw_names[@]}"; do printf '%s\t%s\n' "${fw_names[$i]}" "${fw_shas[$i]}"; done)
    { head -n 1 "$new_digests"
      # via the environment: macOS awk refuses a newline inside a -v value
      { [ -f "$digests" ] && TESTED=$tested awk -F'\t' 'BEGIN { n = split(ENVIRON["TESTED"], a, "\n"); for (i = 1; i <= n; i++) skip[a[i]] = 1 }
                                                        NR > 1 && !(($1 "\t" $2) in skip)' "$digests"
        tail -n +2 "$new_digests"; } | sort -t$'\t' -k1,1 -k3,3
    } > "$digests.tmp" && mv "$digests.tmp" "$digests"
    echo "recorded $(($(wc -l < "$new_digests") - 1)) row(s) in rules-digests.tsv"
fi

if [ "$failures" -gt 0 ]; then
    echo "$failures failure(s); nothing published" >&2
    exit 1
fi

if [ "$site" = 1 ]; then
    rm -rf "$root/site"
    "$csdk" repo build --name "tesla-fw-patches" "$root/dist/public" "$root/site"
    echo "site/ is ready: upload it as-is to a public HTTPS host (port 443)"
fi
count() { find "$root/dist/$1" -maxdepth 1 -name '*.csp' | wc -l | tr -d ' '; }
echo "ok: dist/public ($(count public) publishable), dist/private ($(count private) never publish)"
