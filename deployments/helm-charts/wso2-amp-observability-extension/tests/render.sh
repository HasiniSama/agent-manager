#!/usr/bin/env bash
# Render assertions for wso2-amp-observability-extension.
# Run: bash deployments/helm-charts/wso2-amp-observability-extension/tests/render.sh
#
# Covers the derived auth values that plain `helm template` with default values
# cannot distinguish from hardcoded ones: oauth.authorizationServers falling back
# to auth.issuer, and publicUrl being appended to auth.audience. A wrong issuer
# or audience here is invisible at install time and surfaces only as the observer
# 401ing every traces request with "invalid issuer" (see issue #1424).
set -uo pipefail

CHART_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAILURES=0

# env_value <name> [helm --set args...] -> the rendered value of container env var <name>
# A render failure is reported rather than silently becoming an empty value: this
# chart deliberately `fail`s on some inputs, so a crash must not look like a
# wrong value.
env_value() {
  local name="$1" rendered
  shift
  if ! rendered="$(helm template test-release "$CHART_DIR" "$@" 2>&1)"; then
    printf 'helm template failed: %s\n' "$rendered" >&2
    return 1
  fi
  awk -v n="$name" '
    $1 == "-" && $2 == "name:" && $3 == n { found = 1; next }
    found && $1 == "value:" {
      sub(/^[[:space:]]*value:[[:space:]]*/, "")
      gsub(/^"|"$/, "")
      print
      exit
    }
  ' <<<"$rendered"
}

assert_env() {
  local label="$1" name="$2" expected="$3"
  shift 3
  local actual
  actual="$(env_value "$name" "$@")"
  if [[ "$expected" == "$actual" ]]; then
    printf 'ok   - %s\n' "$label"
  else
    printf 'FAIL - %s\n      expected %s: %q\n      actual   %s: %q\n' \
      "$label" "$name" "$expected" "$name" "$actual"
    FAILURES=$((FAILURES + 1))
  fi
}

# Defaults must stay byte-identical to the pre-derivation literals, so that
# existing installs and quick-start (which passes no overrides) are unaffected.
assert_env "default audience keeps the k3d resource entry" \
  KEY_MANAGER_AUDIENCE "urn:wso2:amp,amp-api-client,am-obs-mcp,http://traces.amp.localhost:11080/mcp"
assert_env "default authorization servers fall back to the default issuer" \
  OAUTH_AUTHORIZATION_SERVERS "http://thunder.amp.localhost:8080"

# The issue #1424 fix: one issuer override has to move the advertised
# authorization server too, or MCP clients discover an authorization server whose
# tokens this service then rejects.
assert_env "issuer override moves the advertised authorization server" \
  OAUTH_AUTHORIZATION_SERVERS "https://thunder.example.com" \
  --set amObserver.auth.issuer=https://thunder.example.com
assert_env "explicit authorization servers win over the issuer" \
  OAUTH_AUTHORIZATION_SERVERS "https://as.example.com" \
  --set amObserver.auth.issuer=https://thunder.example.com \
  --set amObserver.oauth.authorizationServers=https://as.example.com

# publicUrl carries the RFC 8707 resource identifier MCP tokens are minted with,
# so it must reach the audience list with "/mcp" appended and no trailing slash
# (per the MCP spec's canonical-URI guidance).
assert_env "publicUrl override is appended to the audience" \
  KEY_MANAGER_AUDIENCE "urn:wso2:amp,amp-api-client,am-obs-mcp,https://traces.example.com/mcp" \
  --set amObserver.publicUrl=https://traces.example.com
assert_env "a publicUrl that already ends in a slash is not doubled" \
  KEY_MANAGER_AUDIENCE "urn:wso2:amp,amp-api-client,am-obs-mcp,https://traces.example.com/mcp" \
  --set amObserver.publicUrl=https://traces.example.com/
assert_env "an audience that already lists the resource gains no duplicate" \
  KEY_MANAGER_AUDIENCE "amp,https://traces.example.com/mcp" \
  --set amObserver.publicUrl=https://traces.example.com \
  --set 'amObserver.auth.audience=amp\,https://traces.example.com/mcp'
assert_env "whitespace in the audience does not defeat the duplicate check" \
  KEY_MANAGER_AUDIENCE "amp,https://traces.example.com/mcp" \
  --set amObserver.publicUrl=https://traces.example.com \
  --set 'amObserver.auth.audience=amp\, https://traces.example.com/mcp'
assert_env "an empty publicUrl appends nothing and leaves no trailing comma" \
  KEY_MANAGER_AUDIENCE "urn:wso2:amp,amp-api-client,am-obs-mcp" \
  --set-string amObserver.publicUrl=
assert_env "a stray comma in the audience does not produce an empty entry" \
  KEY_MANAGER_AUDIENCE "amp,https://traces.example.com/mcp" \
  --set amObserver.publicUrl=https://traces.example.com \
  --set 'amObserver.auth.audience=amp\,'

# --- global.ampImageRegistry / global.imagePullSecrets -----------------------
# A private-registry install needs both: the image redirected, and a pull secret
# on the pod. Neither is visible from a default `helm template`.

# rendered_field <awk-pattern> [helm --set args...] -> first matching field value
observer_image() {
  local rendered
  if ! rendered="$(helm template test-release "$CHART_DIR" "$@" 2>&1)"; then
    printf 'helm template failed: %s\n' "$rendered" >&2
    return 1
  fi
  awk '$1 == "-" && $2 == "name:" && $3 == "observer" { f = 1; next }
       f && $1 == "image:" { gsub(/^"|"$/, "", $2); print $2; exit }' <<<"$rendered"
}

assert_image() {
  local label="$1" expected="$2"
  shift 2
  local actual
  actual="$(observer_image "$@")"
  if [[ "$expected" == "$actual" ]]; then
    printf 'ok   - %s\n' "$label"
  else
    printf 'FAIL - %s\n      expected: %q\n      actual:   %q\n' "$label" "$expected" "$actual"
    FAILURES=$((FAILURES + 1))
  fi
}

APP_VERSION="$(awk '/^appVersion:/ {gsub(/"/, "", $2); print $2}' "$CHART_DIR/Chart.yaml")"
IMAGE_TAG="$(awk '/^  *tag:/ {gsub(/"/, "", $2); print $2; exit}' "$CHART_DIR/values.yaml")"
: "${IMAGE_TAG:=$APP_VERSION}"

assert_image "default keeps the public observer repository" \
  "ghcr.io/wso2/amp-observer:${IMAGE_TAG}"
assert_image "override redirects the observer image" \
  "registry.example.com/my-org/amp-observer:${IMAGE_TAG}" \
  --set global.ampImageRegistry=registry.example.com/my-org
assert_image "a trailing slash does not double up" \
  "registry.example.com/my-org/amp-observer:${IMAGE_TAG}" \
  --set global.ampImageRegistry=registry.example.com/my-org/
# developmentMode deliberately bypasses the registry: it uses a locally built
# image, so redirecting it at a remote registry would break local debugging.
assert_image "developmentMode is not redirected" \
  "amp-observer:0.0.0-dev" \
  --set amObserver.developmentMode=true \
  --set global.ampImageRegistry=registry.example.com/my-org

# The pod must carry no imagePullSecrets key at all by default: an empty one is
# a rendered zero value that GitOps diffs pick up on every existing install.
if helm template test-release "$CHART_DIR" | grep -q "imagePullSecrets"; then
  printf 'FAIL - default render should not mention imagePullSecrets\n'
  FAILURES=$((FAILURES + 1))
else
  printf 'ok   - default render omits imagePullSecrets entirely\n'
fi
if helm template test-release "$CHART_DIR" \
     --set 'global.imagePullSecrets[0]=wso2-registry-credentials' \
   | grep -A1 'imagePullSecrets:' | grep -q -- '- name: wso2-registry-credentials'; then
  printf 'ok   - a configured pull secret reaches the pod spec\n'
else
  printf 'FAIL - a configured pull secret did not reach the pod spec\n'
  FAILURES=$((FAILURES + 1))
fi

# --- scoreStore ---------------------------------------------------------------
# Off by default, so a default render cannot show that the bundled and external
# modes render the right objects, or that the schema refuses an unsafe mode.

# rendered_objects [helm --set args...] -> one "Kind/name" line per rendered object
rendered_objects() {
  local rendered
  if ! rendered="$(helm template test-release "$CHART_DIR" "$@" 2>&1)"; then
    printf 'helm template failed: %s\n' "$rendered" >&2
    return 1
  fi
  awk '/^kind: / { kind = $2 } /^  name: / && kind != "" { print kind "/" $2; kind = "" }' <<<"$rendered"
}

# assert_objects <label> <present|absent> <objects> <Kind/name>...
assert_objects() {
  local label="$1" want="$2" objects="$3" object missing=""
  shift 3
  for object in "$@"; do
    if grep -qxF "$object" <<<"$objects"; then
      [[ "$want" == absent ]] && missing+=" $object"
    else
      [[ "$want" == present ]] && missing+=" $object"
    fi
  done
  if [[ -z "$missing" ]]; then
    printf 'ok   - %s\n' "$label"
  else
    printf 'FAIL - %s\n      unexpectedly %s:%s\n' "$label" "$([[ "$want" == present ]] && echo missing || echo present)" "$missing"
    FAILURES=$((FAILURES + 1))
  fi
}

# assert_schema_rejects <label> [helm --set args...]
assert_schema_rejects() {
  local label="$1" out
  shift
  if out="$(helm template test-release "$CHART_DIR" "$@" 2>&1)"; then
    printf 'FAIL - %s\n      the render succeeded\n' "$label"
    FAILURES=$((FAILURES + 1))
  elif grep -q "values don't meet the specifications of the schema" <<<"$out"; then
    printf 'ok   - %s\n' "$label"
  else
    printf 'FAIL - %s\n      failed for another reason: %s\n' "$label" "$out"
    FAILURES=$((FAILURES + 1))
  fi
}

SCORE_STORE_OBJECTS=(
  StatefulSet/amp-score-store-postgresql
  Certificate/amp-score-store-postgresql-tls
  Secret/amp-score-store-admin
  Secret/amp-score-store-writer
  Secret/amp-score-store-reader
  NetworkPolicy/amp-score-store-postgresql
)
EXTERNAL_ARGS=(
  --set scoreStore.enabled=true
  --set scoreStore.postgresql.enabled=false
  --set scoreStore.external.host=scores.db.example.com
  --set scoreStore.external.caSecret=score-db-ca
)

objects="$(rendered_objects)"
assert_objects "default render has no score store" absent "$objects" \
  "${SCORE_STORE_OBJECTS[@]}" Certificate/amp-score-store-ca Job/amp-score-store-migration \
  ConfigMap/amp-score-store-migrations

objects="$(rendered_objects --set scoreStore.enabled=true)"
assert_objects "scoreStore.enabled renders the bundled Postgres, CA, Secrets, policy and Job" present "$objects" \
  "${SCORE_STORE_OBJECTS[@]}" Certificate/amp-score-store-ca Job/amp-score-store-migration \
  ConfigMap/amp-score-store-migrations
assert_env "the bundled Job connects to the bundled Postgres" \
  PGHOST "amp-score-store-postgresql.default.svc.cluster.local" \
  --set scoreStore.enabled=true --namespace default

objects="$(rendered_objects "${EXTERNAL_ARGS[@]}" --set scoreStore.external.adminSecret=score-db-admin)"
assert_objects "external mode renders no bundled Postgres or generated Secrets" absent "$objects" \
  "${SCORE_STORE_OBJECTS[@]}"
assert_objects "external mode with an admin Secret runs the migration Job" present "$objects" \
  Job/amp-score-store-migration
assert_env "the external Job connects to the external host" \
  PGHOST "scores.db.example.com" \
  "${EXTERNAL_ARGS[@]}" --set scoreStore.external.adminSecret=score-db-admin

objects="$(rendered_objects "${EXTERNAL_ARGS[@]}")"
assert_objects "external mode without an admin Secret runs no migration Job" absent "$objects" \
  Job/amp-score-store-migration

assert_schema_rejects "schema rejects an sslMode other than verify-full" \
  "${EXTERNAL_ARGS[@]}" --set scoreStore.external.sslMode=require
assert_schema_rejects "schema rejects external mode with an empty caSecret" \
  "${EXTERNAL_ARGS[@]}" --set-string scoreStore.external.caSecret=
assert_schema_rejects "schema rejects retentionDays under 1" \
  --set scoreStore.retentionDays=0
assert_schema_rejects "schema rejects the bundled Postgres without scoreStore.enabled" \
  --set scoreStore.postgresql.enabled=true

# The Job applies what the ConfigMap carries, so every SQL file must reach it.
configmap="$(helm template test-release "$CHART_DIR" --set scoreStore.enabled=true \
  -s templates/score-store-migration-configmap.yaml 2>&1)"
missing_sql=""
for sql in "$CHART_DIR"/files/score-store/migrations/*.sql; do
  grep -q "^  ${sql##*/}: |" <<<"$configmap" || missing_sql+=" ${sql##*/}"
done
if [[ -z "$missing_sql" ]]; then
  printf 'ok   - the migration ConfigMap carries every SQL file\n'
else
  printf 'FAIL - the migration ConfigMap is missing:%s\n' "$missing_sql"
  FAILURES=$((FAILURES + 1))
fi

if ((FAILURES > 0)); then
  printf '\n%d assertion(s) failed\n' "$FAILURES"
  exit 1
fi
printf '\nAll render assertions passed\n'
