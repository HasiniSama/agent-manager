#!/usr/bin/env bash
# Render assertions for wso2-amp-evaluation-extension.
# The NetworkPolicy is matched post-DNAT: a rule naming a Service's address or port matches nothing.
set -uo pipefail

CHART_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAILURES=0

# apiserver_rule [helm --set args...] -> "<cidr,...>|<port,...>" for the egress
# rule carrying ipBlocks, or "" when no such rule renders.
apiserver_rule() {
  local rendered
  if ! rendered="$(helm template test-release "$CHART_DIR" "$@" 2>&1)"; then
    printf 'helm template failed: %s\n' "$rendered" >&2
    return 1
  fi
  printf '%s' "$rendered" | python3 -c '
import sys, yaml
for doc in yaml.safe_load_all(sys.stdin):
    if not doc or doc.get("kind") != "NetworkPolicy":
        continue
    if doc["metadata"]["name"] != "amp-evaluation-job-egress":
        continue
    for rule in doc["spec"].get("egress", []) or []:
        tos = rule.get("to") or []
        ports = [str(p["port"]) for p in rule.get("ports") or []]
        # A rule with ports but no destinations allows those ports everywhere.
        # Report it rather than skipping, so it can never read as "no rule".
        if not tos and ports:
            print("ANY|" + ",".join(ports))
            continue
        blocks = [t["ipBlock"]["cidr"] for t in tos if "ipBlock" in t]
        # The dev-egress rule is also an ipBlock; the API-server rule is the one
        # that does not carry the dev-egress port.
        if blocks and "8080" not in ports:
            print(",".join(blocks) + "|" + ",".join(ports))
'
}

assert_rule() {
  local label="$1" expected="$2"
  shift 2
  local actual
  # Without this, a render failure returns empty and reads as a passing "no rule" case.
  if ! actual="$(apiserver_rule "$@")"; then
    printf 'FAIL - %s\n      render failed\n' "$label"
    FAILURES=$((FAILURES + 1))
    return
  fi
  if [[ "$expected" == "$actual" ]]; then
    printf 'ok   - %s\n' "$label"
  else
    printf 'FAIL - %s\n      expected: %q\n      actual:   %q\n' "$label" "$expected" "$actual"
    FAILURES=$((FAILURES + 1))
  fi
}

# The defect this guards: a default render that silently omits the rule.
assert_rule "default render carries the API-server rule on both control-plane ports" \
  "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16|443,6443"

# Helm must replace the default list, not merge into it — a merge would silently re-admit
# the pod and service CIDRs, so exactly one ipBlock has to survive.
assert_rule "an explicit cidrs[0] replaces the permissive default outright" \
  "172.19.0.0/16|443,6443" \
  --set "networkPolicy.evaluationJob.apiServer.cidrs[0]=172.19.0.0/16"

# An empty CIDR must drop the entry rather than render `cidr: null`.
assert_rule "an empty cidr entry renders no ipBlock" \
  "" \
  --set "networkPolicy.evaluationJob.apiServer.cidrs[0]="

# Changing a number here means checking the target's containerPort, not its Service port.
ns_rule_ports() {
  helm template test-release "$CHART_DIR" 2>/dev/null | python3 -c '
import sys, yaml
for doc in yaml.safe_load_all(sys.stdin):
    if not doc or doc.get("kind") != "NetworkPolicy":
        continue
    if doc["metadata"]["name"] != "amp-evaluation-job-egress":
        continue
    for rule in doc["spec"].get("egress", []) or []:
        names = []
        for t in rule.get("to") or []:
            ns = (t.get("namespaceSelector") or {}).get("matchLabels") or {}
            names += list(ns.values())
        if not names:
            continue
        ports = ",".join(str(p["port"]) for p in rule.get("ports") or [])
        print("+".join(names) + "=" + (ports if ports else "ALL"))
'
}

expected_ns_ports="kube-system=53,53
openchoreo-observability-plane=9098
amp-thunder=8090
wso2-amp=8080
true=22893"

actual_ns_ports="$(ns_rule_ports)"
if [[ "$expected_ns_ports" == "$actual_ns_ports" ]]; then
  printf 'ok   - every namespaceSelector rule targets its containerPort\n'
else
  printf 'FAIL - every namespaceSelector rule targets its containerPort\n      expected:\n%s\n      actual:\n%s\n' \
    "$expected_ns_ports" "$actual_ns_ports"
  FAILURES=$((FAILURES + 1))
fi

# --- global.ampImageRegistry / global.imagePullSecrets -----------------------
# The evaluation image is the awkward one: its pod runs as an Argo workflow in
# the workflow namespace, not this chart's release namespace, and the repository
# is assembled by a helper with two branches. Both branches need pinning.

job_image() {
  local rendered
  if ! rendered="$(helm template test-release "$CHART_DIR" "$@" 2>&1)"; then
    printf 'helm template failed: %s\n' "$rendered" >&2
    return 1
  fi
  awk '$1 == "image:" { v = $2; gsub(/^['"'"'"]|['"'"'"]$/, "", v); print v; exit }' <<<"$rendered"
}

assert_image() {
  local label="$1" expected="$2"
  shift 2
  local actual
  actual="$(job_image "$@")"
  if [[ "$expected" == "$actual" ]]; then
    printf 'ok   - %s\n' "$label"
  else
    printf 'FAIL - %s\n      expected: %q\n      actual:   %q\n' "$label" "$expected" "$actual"
    FAILURES=$((FAILURES + 1))
  fi
}

TAG="$(awk '/^    tag:/ {gsub(/"/, "", $2); print $2; exit}' "$CHART_DIR/values.yaml")"

assert_image "default keeps the public evaluation repository" \
  "ghcr.io/wso2/amp-evaluation-monitor:${TAG}"
assert_image "override redirects the evaluation image" \
  "registry.example.com/my-org/amp-evaluation-monitor:${TAG}" \
  --set global.ampImageRegistry=registry.example.com/my-org
assert_image "a trailing slash does not double up" \
  "registry.example.com/my-org/amp-evaluation-monitor:${TAG}" \
  --set global.ampImageRegistry=registry.example.com/my-org/
# useLocalRegistry prefixes the in-cluster build registry instead; a remote
# override there would defeat the point of importing a locally built image.
assert_image "useLocalRegistry ignores the override" \
  "host.k3d.internal:10082/ghcr.io/wso2/amp-evaluation-monitor:${TAG}" \
  --set ampEvaluation.useLocalRegistry=true \
  --set global.ampImageRegistry=registry.example.com/my-org

# The pull secret has to reach BOTH specs: the ClusterWorkflowTemplate (used on a
# direct submission) and the Workflow's runTemplate (the production path, whose
# spec wins over the referenced template).
if helm template test-release "$CHART_DIR" | grep -q "imagePullSecrets"; then
  printf 'FAIL - default render should not mention imagePullSecrets\n'
  FAILURES=$((FAILURES + 1))
else
  printf 'ok   - default render omits imagePullSecrets entirely\n'
fi
PULL_COUNT="$(helm template test-release "$CHART_DIR" \
  --set 'global.imagePullSecrets[0]=wso2-registry-credentials' \
  | grep -c -- '- name: wso2-registry-credentials' || true)"
if [[ "$PULL_COUNT" == "2" ]]; then
  printf 'ok   - the pull secret reaches both workflow specs\n'
else
  printf 'FAIL - expected the pull secret in 2 specs, found %s\n' "$PULL_COUNT"
  FAILURES=$((FAILURES + 1))
fi

# --- ampEvaluation.scoreWriter ------------------------------------------------
# Off by default: nothing about the writer renders. On, the job gets the flags,
# its container alone mounts the CA, and egress opens to the writer's port.

WRITER_URL="https://amp-score-writer.openchoreo-observability-plane.svc.cluster.local:9099"

check() {
  local label="$1"
  shift
  if "$@"; then
    printf 'ok   - %s\n' "$label"
  else
    printf 'FAIL - %s\n' "$label"
    FAILURES=$((FAILURES + 1))
  fi
}

writer_render() {
  helm template test-release "$CHART_DIR" "$@" 2>&1 | python3 -c '
import sys, yaml
docs = [d for d in yaml.safe_load_all(sys.stdin) if d]
wf = next(d for d in docs if d["kind"] == "ClusterWorkflowTemplate")
container = wf["spec"]["templates"][0]["container"]
np = next(d for d in docs if d["kind"] == "NetworkPolicy")
args = [a for a in container["args"] if a.startswith("--score-writer")]
mounts = [m["mountPath"] for m in container.get("volumeMounts", [])]
volumes = [v["secret"]["secretName"] for v in wf["spec"].get("volumes", [])]
rules = []
for rule in np["spec"]["egress"]:
    ports = [str(p["port"]) for p in rule.get("ports") or []]
    for t in rule.get("to") or []:
        ns = ((t.get("namespaceSelector") or {}).get("matchLabels") or {}).get("kubernetes.io/metadata.name")
        cidr = (t.get("ipBlock") or {}).get("cidr")
        if ports in (["9099"], ["8443"]):
            rules.append((ns or cidr) + "=" + ",".join(ports))
print(" ".join(args) + "|" + ",".join(mounts) + "|" + ",".join(volumes) + "|" + ";".join(rules))
'
}

check "default render mentions no score writer" \
  bash -c "! helm template test-release '$CHART_DIR' | grep -qi 'score-writer\|scoreWriter\|9099'"

check "endpoint and CA render the flags, the mount and the egress rule" \
  test "$(writer_render --set "ampEvaluation.scoreWriter.endpoint=$WRITER_URL" \
    --set ampEvaluation.scoreWriter.caSecret=amp-score-store-ca)" == \
  "--score-writer-endpoint=$WRITER_URL --score-writer-ca=/var/run/amp/ca/ca.crt|/var/run/amp/ca|amp-score-store-ca|openchoreo-observability-plane=9099"

check "endpoint without a CA mounts nothing" \
  test "$(writer_render --set "ampEvaluation.scoreWriter.endpoint=$WRITER_URL")" == \
  "--score-writer-endpoint=$WRITER_URL|||openchoreo-observability-plane=9099"

check "a cidr replaces the namespace for a gateway outside the cluster" \
  test "$(writer_render --set ampEvaluation.scoreWriter.endpoint=https://scores.example.com \
    --set networkPolicy.evaluationJob.scoreWriter.cidr=203.0.113.10/32 \
    --set 'networkPolicy.evaluationJob.scoreWriter.ports[0]=8443')" == \
  "--score-writer-endpoint=https://scores.example.com|||203.0.113.10/32=8443"

check "the schema refuses a plain http endpoint" \
  bash -c "! helm template test-release '$CHART_DIR' --set ampEvaluation.scoreWriter.endpoint=http://amp-score-writer:9099 >/dev/null 2>&1"

if [[ $FAILURES -gt 0 ]]; then
  printf '\nwso2-amp-evaluation-extension: %d render assertion(s) failed\n' "$FAILURES"
  exit 1
fi
printf '\nwso2-amp-evaluation-extension: render assertions passed\n'
