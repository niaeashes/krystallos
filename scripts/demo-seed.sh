#!/usr/bin/env bash
# Seed a running gitsynth with a small demo: an org and a repository with a
# few commits, an agent identity for the admin, an idea turned into an issue,
# a plan written by the agent, and the agent's change set waiting for review.
#
#   scripts/demo-seed.sh ADMIN_TOKEN [SERVER_URL]
#
# ADMIN_TOKEN is the token `gitsynth admin init` printed.
set -euo pipefail

ADMIN=${1:?usage: scripts/demo-seed.sh ADMIN_TOKEN [SERVER_URL]}
BASE=${2:-http://127.0.0.1:3000}
export NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost

api() { # api TOKEN METHOD PATH [JSON]
  local args=(-sS --fail-with-body -H "Authorization: Bearer $1" -X "$2" "$BASE$3")
  if [ $# -ge 4 ]; then args+=(-H 'Content-Type: application/json' -d "$4"); fi
  curl "${args[@]}"
}
field() { python3 -c 'import json,sys; print(json.load(sys.stdin)[sys.argv[1]])' "$1"; }
git_as() { # git_as TOKEN args...
  local token=$1
  shift
  git -c credential.helper= -c "http.extraHeader=Authorization: Bearer $token" "$@"
}

me=$(api "$ADMIN" GET /api/me | field identity) # user:NAME
name=${me#user:}
api "$ADMIN" POST /api/orgs '{"name":"acme"}' >/dev/null
api "$ADMIN" POST /api/orgs/acme/repos '{"name":"app"}' >/dev/null
url="$BASE/git/acme/app.git"

work=$(mktemp -d "${TMPDIR:-/tmp}/gitsynth-seed.XXXXXX")
trap 'rm -rf "$work"' EXIT
git_as "$ADMIN" -c init.defaultBranch=main clone -q "$url" "$work/app" 2>/dev/null
cd "$work/app"
git config user.name "$name"
git config user.email "$name@example.test"
mkdir -p src
printf '# app\n\nA small demo service.\n' >README.md
printf 'package main\n\nfunc main() {\n}\n' >src/main.go
git add -A
git commit -qm "Start the service"
git_as "$ADMIN" push -q origin HEAD:main

# The admin's agent gets its own identity, write access but no merge, and a
# standing token. The agent's run opens a session with it and works through
# the session's token.
api "$ADMIN" POST /api/identities '{"label":"claude"}' >/dev/null
api "$ADMIN" POST /api/repos/acme/app/grants "{\"identity\":\"$me/claude\",\"set\":\"write\"}" >/dev/null
STANDING=$(api "$ADMIN" POST /api/tokens "{\"identity\":\"$me/claude\",\"ttl_hours\":24}" | field token)
AGENT=$(api "$STANDING" POST /api/sessions '{"label":"demo-session-1"}' | field token)

api "$ADMIN" POST /api/orgs/acme/ideas '{"title":"Greet people","body":"The service should say hello.","repos":["app"]}' >/dev/null
api "$ADMIN" POST /api/orgs/acme/ideas/1/convert '{"repo":"app"}' >/dev/null
api "$AGENT" POST /api/orgs/acme/ideas '{"title":"The config loader should be rebuilt","body":"Noticed while working on the greeting; out of scope there.","repos":["app"]}' >/dev/null
api "$AGENT" POST /api/repos/acme/app/issues/1/plans \
  '{"title":"Print a greeting from main","body":"Keep it small.","steps":[{"title":"Add a greet function"},{"title":"Call it from main"},{"title":"Mention it in the README"}]}' >/dev/null

# The agent commits with its own address, registered to its identity, so
# the server can tell its commits from the admin's.
api "$AGENT" POST /api/emails "{\"email\":\"$name+claude@example.test\"}" >/dev/null

git checkout -qb greet
printf 'package main\n\nimport "fmt"\n\nfunc greet(name string) string {\n\treturn fmt.Sprintf("hello, %%s", name)\n}\n' >src/greet.go
git add -A
git -c user.name=claude -c "user.email=$name+claude@example.test" commit -qm "Add greet"
printf 'package main\n\nimport "fmt"\n\nfunc main() {\n\tfmt.Println(greet("world"))\n}\n' >src/main.go
git -c user.name=claude -c "user.email=$name+claude@example.test" commit -qam "Greet from main"
git_as "$AGENT" push -q origin greet

api "$AGENT" POST /api/repos/acme/app/issues/1/changesets \
  '{"title":"Greet from main","branch":"greet","plan":1,"steps":["s1","s2"]}' >/dev/null
api "$ADMIN" PUT /api/repos/acme/app/merge-rule '{"required_approvals":1,"required_checks":[]}' >/dev/null

# A bot the admin owns, with an identity the admin derives for its triage
# work, which handles an alert in a session of its own; and CI as a service.
api "$ADMIN" POST /api/bots '{"name":"devbox"}' >/dev/null
api "$ADMIN" POST /api/repos/acme/app/grants '{"identity":"bot:devbox","set":"write"}' >/dev/null
api "$ADMIN" POST /api/identities '{"parent":"bot:devbox","label":"triage"}' >/dev/null
TRIAGE_STANDING=$(api "$ADMIN" POST /api/tokens '{"identity":"bot:devbox/triage"}' | field token)
TRIAGE=$(api "$TRIAGE_STANDING" POST /api/sessions '{"label":"alert-42"}' | field token)
api "$TRIAGE" POST /api/orgs/acme/ideas '{"title":"Error rate doubled after the last deploy","repos":["app"]}' >/dev/null
api "$ADMIN" POST /api/services '{"name":"ci","description":"Runs the tests","permissions":["repo.read","check.write"],"events":["git.ref_transaction"]}' >/dev/null
api "$ADMIN" POST /api/orgs/acme/services '{"service":"service:ci"}' >/dev/null

echo "seeded acme/app: issue 1 has a plan and an open change set by $me/claude; bot:devbox and service:ci exist"
