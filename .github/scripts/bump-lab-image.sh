#!/usr/bin/env bash
# Pin one image tag in the lab values file and push the change to main.
#
#   bump-lab-image.sh <marker> <tag>
#
# The values file marks each CI-managed line with a trailing comment
# "# ci: <marker>", e.g.
#     tag: main-4c3abc8  # ci: backend-image
# Only that line is rewritten, so hand edits elsewhere are untouched.
#
# Runs after the image push, so Argo CD only sees the new tag once the image
# exists. The commit carries [skip ci] so it does not trigger another build.
set -euo pipefail

MARKER="$1"
TAG="$2"
FILE="deploy/helm/networkdoctor/values-onprem-lab.yaml"

grep -q "# ci: ${MARKER}\$" "$FILE" || { echo "no '# ci: ${MARKER}' line in $FILE" >&2; exit 1; }
sed -i -E "s|^([[:space:]]*tag:)[[:space:]]*[^#[:space:]]+([[:space:]]+# ci: ${MARKER})\$|\1 ${TAG}\2|" "$FILE"

if git diff --quiet -- "$FILE"; then
  echo "already at ${TAG}"
  exit 0
fi

git config user.name "github-actions[bot]"
git config user.email "41898282+github-actions[bot]@users.noreply.github.com"
git add "$FILE"
git commit -m "chore(lab): ${MARKER} tag -> ${TAG} [skip ci]"

# The agent and backend workflows can finish at the same time; rebase and retry.
for i in 1 2 3 4 5; do
  if git push origin HEAD:main; then exit 0; fi
  sleep $((i * 3))
  git pull --rebase origin main
done
echo "push failed after retries" >&2
exit 1
