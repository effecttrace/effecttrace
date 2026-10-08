#!/usr/bin/env bash
# Pre-push hygiene scan: tracked files, commit messages and tags must not
# contain personal data, credentials, private paths or development-tool
# attribution. Matches are printed masked. Exits non-zero on any finding.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

fail=0
report() { # file:line, label, match
  local m="$3"
  local masked="${m:0:3}***${m: -2}"
  printf '  %-12s %s  %s\n' "$2" "$1" "$masked"
  fail=1
}

# Patterns are assembled from fragments so this file does not match itself.
home="/Us""ers/sand""eepbazar"
patterns=(
  "personal-path|${home}"
  "email|[A-Za-z0-9._%+-]+@(ib""m\.com|ii""mu\.ac\.in|gm""ail\.com)"
  "email|sand""eepbazar@"
  "private-key|-----BEGIN [A-Z ]*PRIV""ATE KEY-----"
  "token|(gh[pousr]_[A-Za-z0-9]{30,}|github_p""at_[A-Za-z0-9_]{40,}|AK""IA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,})"
  "kubeconfig|client-(key|certificate)-d""ata: [A-Za-z0-9+/=]{20,}"
  "auth-header|Authorization: Bearer [A-Za-z0-9._~+/=-]{20,}"
  "phone|(^|[^0-9A-Za-z_.:-])(\+[0-9]{1,3}[ -][0-9]{3,5}[ -][0-9]{3,5}([ -][0-9]{2,5})?|\([0-9]{3}\) ?[0-9]{3}-[0-9]{4}|[6-9][0-9]{4}[ -][0-9]{5})([^0-9A-Za-z]|$)"
)
attribution='(Clau''de|Anthro''pic|Cod''ex|Open''AI|Antigra''vity|\bB''ob\b|Copi''lot|Co-auth''ored-by|Generat''ed-by|AI-gener''ated|AI-assi''sted)'

echo "scanning tracked files"
files=()
while IFS= read -r f; do files+=("$f"); done < <(git ls-files | grep -v -E '^scripts/scan\.sh$' | grep -v -E '\.(png|jpg|jpeg|gif|ico|woff2?)$')
if ((${#files[@]})); then
  for p in "${patterns[@]}"; do
    label="${p%%|*}"; re="${p#*|}"
    while IFS= read -r hit; do
      [[ -z "$hit" ]] && continue
      loc="${hit%%:*}:$(cut -d: -f2 <<<"$hit")"
      match="$(grep -oE -e "$re" <<<"${hit#*:*:}" | head -1)"
      report "$loc" "$label" "$match"
    done < <(grep -nE -e "$re" -- "${files[@]}" 2>/dev/null || true)
  done
  while IFS= read -r hit; do
    [[ -z "$hit" ]] && continue
    loc="${hit%%:*}:$(cut -d: -f2 <<<"$hit")"
    match="$(grep -oE -e "$attribution" <<<"${hit#*:*:}" | head -1)"
    report "$loc" "attribution" "$match"
  done < <(grep -nE -e "$attribution" -- "${files[@]}" 2>/dev/null || true)
fi

echo "checking that no private context files are tracked"
if git ls-files | grep -E '(^|/)context/' >/dev/null; then
  git ls-files | grep -E '(^|/)context/' | while read -r f; do report "$f" "context" "$f"; done
  fail=1
fi

if git rev-parse --verify HEAD >/dev/null 2>&1; then
  echo "scanning commit messages and tags"
  msgs="$(git log --format='%H %B' --all)"
  if grep -nE -e "$attribution" <<<"$msgs" >/dev/null; then
    grep -nE -e "$attribution" <<<"$msgs" | while read -r hit; do report "commit-msg:${hit%%:*}" "attribution" "$(grep -oE -e "$attribution" <<<"$hit" | head -1)"; done
    fail=1
  fi
  for p in "${patterns[@]}"; do
    re="${p#*|}"
    if grep -nE -e "$re" <<<"$msgs" >/dev/null; then report "commit-msg" "${p%%|*}" "$(grep -oE -e "$re" <<<"$msgs" | head -1)"; fi
  done
  authors="$(git log --all --format='%an <%ae>|%cn <%ce>' | tr '|' '\n' | sort -u)"
  while read -r a; do
    [[ -z "$a" ]] && continue
    if ! grep -qE '^sandeepbazar <[0-9]+\+sandeepbazar@users\.noreply\.github\.com>$|^GitHub <noreply@github\.com>$' <<<"$a"; then
      report "commit-identity" "identity" "$a"
    fi
  done <<<"$authors"
  if git tag -l | grep -qE -e "$attribution"; then report "tags" "attribution" "$(git tag -l | grep -oE -e "$attribution" | head -1)"; fi
fi

if ((fail)); then
  echo "scan FAILED"
  exit 1
fi
echo "scan passed"
