#!/usr/bin/env bash
set -euo pipefail

# Provider-free direct lifecycle contract gate. This exercises Kubernetes
# admission/status storage and the work -> capture -> fresh-verify boundaries
# with ordinary Job/PVC objects. It is not a substitute for a real operator
# run; see docs/v3/direct-live-gate.md.

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
kubectl_bin=${AGW_DIRECT_KUBECTL_BIN:-kubectl}
context=${AGW_DIRECT_KUBE_CONTEXT:-}
kubeconfig=${AGW_DIRECT_KUBECONFIG:-${KUBECONFIG:-}}
mode=plan
timeout=90
namespace_prefix=${AGW_DIRECT_NAMESPACE_PREFIX:-agw-direct}
keep_namespaces=${AGW_DIRECT_KEEP:-0}

usage() {
	cat <<'EOF'
usage: live-gate.sh [--plan|--live] [options]

Modes:
  --plan              lint and client-parse fixtures (default)
  --live              run on an explicit loopback k3s API server

Options:
  --context NAME      kubeconfig context; required by --live
  --kubeconfig PATH  kubeconfig file; otherwise KUBECONFIG is used
  --timeout SECONDS  per-resource wait bound (default: 90)
  --keep              retain generated namespaces for inspection
  -h, --help         show this help

Environment:
  AGW_DIRECT_KEEP=1  retain the exact generated namespaces for inspection

Live mode refuses non-loopback API servers and unsafe namespace prefixes. The
prefix must be agw-direct or start with agw-direct-, and both generated names
must be valid DNS labels no longer than 63 characters. It deletes only the
exact generated namespaces by default and never deletes CRDs or other
cluster-scoped resources.
EOF
}

die() {
	echo "direct-live-gate: $*" >&2
	exit 1
}

while (($#)); do
	case "$1" in
		--plan) mode=plan; shift ;;
		--live) mode=live; shift ;;
		--context)
			[[ $# -ge 2 ]] || die "--context requires a value"
			context=$2
			shift 2
			;;
		--kubeconfig)
			[[ $# -ge 2 ]] || die "--kubeconfig requires a path"
			kubeconfig=$2
			shift 2
			;;
		--timeout)
			[[ $# -ge 2 ]] || die "--timeout requires a value"
			timeout=$2
			shift 2
			;;
		--keep) keep_namespaces=1; shift ;;
		-h|--help) usage; exit 0 ;;
		*) die "unknown argument: $1" ;;
	esac
done

[[ "$timeout" =~ ^[1-9][0-9]*$ ]] || die "timeout must be a positive integer"
[[ "$keep_namespaces" == 0 || "$keep_namespaces" == 1 ]] || die 'AGW_DIRECT_KEEP must be 0 or 1'

fixture="$script_dir/agent-run-fixture.yaml"
resources="$script_dir/contract-resources.yaml"
failure_child="$script_dir/failure-child.yaml"
for file in "$fixture" "$resources" "$failure_child"; do
	[[ -f "$file" ]] || die "missing fixture: $file"
done

validate_dns_label() {
	local value=$1
	local description=$2
	(( ${#value} <= 63 )) || die "$description is longer than 63 characters: $value"
	[[ "$value" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] ||
		die "$description must be a lowercase DNS label: $value"
}

validate_namespace_names() {
	local prefix=$1
	local id=$2
	validate_dns_label "$prefix" 'namespace prefix'
	[[ "$prefix" == agw-direct || "$prefix" == agw-direct-* ]] ||
		die 'namespace prefix must be agw-direct or start with agw-direct-'
	for suffix in success failure; do
		validate_dns_label "${prefix}-${suffix}-${id}" "generated ${suffix} namespace"
	done
}

run_id=$(date -u +%Y%m%d%H%M%S)-$$

render() {
	local file=$1
	sed \
		-e "s/__NAMESPACE__/${case_namespace}/g" \
		-e "s/__RUN_NAME__/${case_name}/g" \
		-e "s/__RUN_UID__/${case_uid}/g" \
		-e "s/__SPEC_DIGEST__/${spec_digest}/g" \
		-e "s/__BASE_SHA__/${base_sha}/g" \
		"$file"
}

render_fixture() {
	local file=$1
	sed \
		-e "s/__NAMESPACE__/${case_namespace}/g" \
		-e "s/__RUN_NAME__/${case_name}/g" \
		"$file"
}

if [[ "$mode" == plan ]]; then
	bash -n "$0"
	for file in "$fixture" "$resources" "$failure_child"; do
		grep -Fq '__NAMESPACE__' "$file" || die "$file has no namespace marker"
		if [[ "$file" != "$fixture" ]]; then
			grep -Fq '__RUN_UID__' "$file" || die "$file has no UID marker"
		fi
	done
	case_namespace=agw-direct-plan
	case_name=direct-plan
	case_uid=00000000-0000-4000-8000-000000000000
	spec_digest=sha256:1111111111111111111111111111111111111111111111111111111111111111
	base_sha=1111111111111111111111111111111111111111
	validate_namespace_names "$namespace_prefix" "$run_id"
	command -v python3 >/dev/null 2>&1 || die 'python3 is required for plan parsing'
	python3 - "$fixture" "$resources" "$failure_child" <<'PY'
import pathlib
import sys
import yaml
from yaml.constructor import ConstructorError


class UniqueKeyLoader(yaml.SafeLoader):
    pass


def construct_unique_mapping(loader, node, deep=False):
    if not isinstance(node, yaml.MappingNode):
        raise ConstructorError(None, None, 'expected a mapping node', node.start_mark)
    mapping = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        try:
            duplicate = key in mapping
        except TypeError as exc:
            raise ConstructorError(
                'while constructing a mapping',
                node.start_mark,
                f'unhashable key: {key!r}',
                key_node.start_mark,
            ) from exc
        if duplicate:
            raise ConstructorError(
                'while constructing a mapping',
                node.start_mark,
                f'duplicate key: {key!r}',
                key_node.start_mark,
            )
        mapping[key] = loader.construct_object(value_node, deep=deep)
    return mapping


UniqueKeyLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG,
    construct_unique_mapping,
)

for raw in sys.argv[1:]:
    path = pathlib.Path(raw)
    text = path.read_text(encoding='utf-8')
    rendered = text.replace('__NAMESPACE__', 'agw-direct-plan').replace('__RUN_NAME__', 'direct-plan').replace('__RUN_UID__', '00000000-0000-4000-8000-000000000000').replace('__SPEC_DIGEST__', 'sha256:' + '1' * 64).replace('__BASE_SHA__', '1' * 40)
    try:
        documents = list(yaml.load_all(rendered, Loader=UniqueKeyLoader))
    except yaml.YAMLError as exc:
        raise SystemExit(f'{path}: invalid YAML: {exc}')
    if not documents or any(document is None for document in documents):
        raise SystemExit(f'{path}: empty YAML document')
    for document in documents:
        if not isinstance(document, dict) or not document.get('apiVersion') or not document.get('kind'):
            raise SystemExit(f'{path}: document lacks apiVersion/kind')

duplicate_fixture = '''apiVersion: v1
kind: ConfigMap
metadata:
  name: duplicate-key
metadata:
  name: duplicate-key-again
'''
try:
    list(yaml.load_all(duplicate_fixture, Loader=UniqueKeyLoader))
except ConstructorError as exc:
    if 'duplicate key' not in str(exc):
        raise SystemExit(f'duplicate-key guard raised the wrong error: {exc}')
else:
    raise SystemExit('duplicate-key guard accepted duplicate YAML mapping keys')
print('fixture YAML parse: PASS')
PY
	echo 'direct-live-gate: PLAN PASS (no API mutation)'
	exit 0
fi

[[ "$mode" == live ]] || die "unsupported mode: $mode"
[[ -n "$context" ]] || die '--context is required in --live mode'
[[ -n "$kubeconfig" ]] || die '--kubeconfig or KUBECONFIG is required in --live mode'
[[ -f "$kubeconfig" ]] || die "kubeconfig does not exist: $kubeconfig"
command -v "$kubectl_bin" >/dev/null 2>&1 || die "kubectl not found: $kubectl_bin"

kubectl=("$kubectl_bin" --kubeconfig "$kubeconfig" --context "$context")
server=$(${kubectl[@]} config view --minify -o jsonpath='{.clusters[0].cluster.server}')
case "$server" in
	https://127.0.0.1:*|https://localhost:*|https://\[::1\]:*) ;;
	*) die "refusing non-loopback API server: $server" ;;
esac
${kubectl[@]} cluster-info >/dev/null

validate_namespace_names "$namespace_prefix" "$run_id"
namespaces=()
cleanup() {
	local status=$?
	local cleanup_status=0
	if (( keep_namespaces )); then
		echo "direct-live-gate: retained namespaces for inspection: ${namespaces[*]:-none}" >&2
		return "$status"
	fi
	for namespace in "${namespaces[@]}"; do
		if ! ${kubectl[@]} delete namespace "$namespace" --ignore-not-found=true --wait=true --timeout="${timeout}s" >/dev/null; then
			echo "direct-live-gate: failed to delete exact namespace: $namespace" >&2
			cleanup_status=1
		fi
	done
	if (( status == 0 && cleanup_status != 0 )); then
		status=$cleanup_status
	fi
	return "$status"
}
trap cleanup EXIT

crds=(
	agents.agents.astatide.com
	agentruns.agents.astatide.com
	gates.agents.astatide.com
	toolsets.agents.astatide.com
	modelroutes.agents.astatide.com
	policies.agents.astatide.com
	contextstrategies.agents.astatide.com
)
for crd in "${crds[@]}"; do
	condition=$(${kubectl[@]} get crd "$crd" -o json | jq -r '[.status.conditions[]? | select(.type == "Established") | .status][0] // ""')
	[[ "$condition" == True ]] || die "CRD did not become Established: $crd"
done
echo 'direct-live-gate: required CRDs already Established (no cluster-scoped mutation)'

fixed_status_digest=sha256:1111111111111111111111111111111111111111111111111111111111111111
fixed_base_sha=1111111111111111111111111111111111111111
fixed_plan_digest=sha256:2222222222222222222222222222222222222222222222222222222222222222
patch_digest=sha256:$(printf '%s\n' 'change=1' | sha256sum | awk '{print $1}')
manifest_digest=sha256:$(printf '%s\n' '{"files":["change.txt"]}' | sha256sum | awk '{print $1}')
report_digest=sha256:$(printf '%s\n' 'direct-verify-report' | sha256sum | awk '{print $1}')

get_uid() {
	${kubectl[@]} get "$1" "$2" -n "$case_namespace" -o jsonpath='{.metadata.uid}'
}

patch_status() {
	local payload=$1
	${kubectl[@]} patch agentrun "$case_name" -n "$case_namespace" --subresource=status --type=merge -p "$payload" >/dev/null
}

wait_deleted() {
	local kind=$1 name=$2 output
	for ((attempt=0; attempt<timeout; attempt++)); do
		if output=$(${kubectl[@]} get "$kind" "$name" -n "$case_namespace" 2>&1); then
			sleep 1
			continue
		fi
		if grep -qiE 'not found|notfound|could not find the requested resource' <<<"$output"; then
			return 0
		fi
		printf '%s\n' "$output" >&2
		return 1
	done
	return 1
}

wait_complete() {
	local name=$1
	if ! ${kubectl[@]} wait --for=condition=complete "job/$name" -n "$case_namespace" --timeout="${timeout}s" >/dev/null; then
		${kubectl[@]} describe job "$name" -n "$case_namespace" >&2 || true
		${kubectl[@]} logs "job/$name" -n "$case_namespace" >&2 || true
		die "$name did not complete"
	fi
}

wait_failed() {
	local name=$1
	${kubectl[@]} wait --for=condition=failed "job/$name" -n "$case_namespace" --timeout="${timeout}s" >/dev/null
}

check_owner() {
	local kind=$1 name=$2 uid=$3
	local body
	body=$(${kubectl[@]} get "$kind" "$name" -n "$case_namespace" -o json)
	printf '%s\n' "$body" | jq -e --arg uid "$uid" --arg name "$case_name" '
		any(.metadata.ownerReferences[]?;
			.apiVersion == "agents.astatide.com/v1alpha1" and
			.kind == "AgentRun" and .name == $name and .uid == $uid and
			.controller == true and .blockOwnerDeletion == true)' >/dev/null ||
		die "$kind/$name is not owned by the exact AgentRun UID"
}

ensure_namespace_absent() {
	local namespace=$1 existing
	validate_dns_label "$namespace" 'generated namespace'
	existing=$(${kubectl[@]} get namespace "$namespace" --ignore-not-found=true -o name 2>&1) ||
		die "could not verify disposable namespace is absent: $namespace"
	[[ -z "$existing" ]] || die "refusing to reuse existing namespace: $namespace"
}

run_success_case() {
	case_namespace="${namespace_prefix}-success-${run_id}"
	case_name=direct-success
	case_uid=
	spec_digest=$fixed_status_digest
	base_sha=$fixed_base_sha
	ensure_namespace_absent "$case_namespace"
	${kubectl[@]} create namespace "$case_namespace" >/dev/null
	namespaces+=("$case_namespace")
	render_fixture "$fixture" | ${kubectl[@]} apply --server-side --field-manager=direct-live-gate -f - >/dev/null
	case_uid=$(get_uid agentrun "$case_name")
	[[ -n "$case_uid" ]] || die 'AgentRun admission returned no UID'

	patch_status "$(jq -cn --arg digest "$spec_digest" --arg base "$base_sha" '{status:{phase:"Pending",observedGeneration:1,specDigest:$digest,baseSHA:$base,conditions:[{type:"Admitted",status:"True",reason:"FixtureAdmitted",message:"offline contract fixture admitted",observedGeneration:1}]}}')"
	[[ "$(${kubectl[@]} get agentrun "$case_name" -n "$case_namespace" -o jsonpath='{.status.phase}')" == Pending ]] || die 'AgentRun status admission projection failed'

	render "$resources" | ${kubectl[@]} apply --server-side --field-manager=direct-live-gate -f - >/dev/null
	for child in direct-work direct-capture direct-verify direct-verify-failure; do
		check_owner job "$child" "$case_uid"
	done
	check_owner configmap direct-expected-patch "$case_uid"
	check_owner pvc direct-workspace "$case_uid"
	check_owner pvc direct-verify-workspace "$case_uid"

	wait_complete direct-work
	work_uid=$(get_uid job direct-work)
	patch_status "$(jq -cn --arg digest "$spec_digest" --arg base "$base_sha" --arg uid "$work_uid" --arg plan "$fixed_plan_digest" '{status:{phase:"Working",observedGeneration:1,specDigest:$digest,baseSHA:$base,workSandboxRef:{name:"direct-work",kind:"Job",uid:$uid,role:"work",specDigest:$digest,planFingerprint:$plan},conditions:[{type:"WorkReady",status:"True",reason:"MockReady",message:"work boundary completed",observedGeneration:1}]}}')"

	wait_complete direct-capture
	capture_logs=$(${kubectl[@]} logs job/direct-capture -n "$case_namespace")
	printf '%s\n' "$capture_logs" | jq -e --arg uid "$case_uid" --arg digest "$spec_digest" 'select(.captured == true and .runUid == $uid and .specDigest == $digest)' >/dev/null || die 'capture evidence was not exact or durable'
	patch_status "$(jq -cn --arg digest "$spec_digest" --arg base "$base_sha" --arg patch "$patch_digest" --arg manifest "$manifest_digest" --arg plan "$fixed_plan_digest" '{status:{phase:"Capturing",observedGeneration:1,specDigest:$digest,baseSHA:$base,workSandboxRef:{name:"direct-work",kind:"Job",uid:"deleted-after-capture",role:"work",specDigest:$digest,planFingerprint:$plan},patch:{ref:{uri:"s3://offline/direct/patch.diff",digest:$patch,kind:"patch",name:"patch.diff",mediaType:"text/plain"},manifestRef:{uri:"s3://offline/direct/manifest.json",digest:$manifest,kind:"patch-manifest",name:"manifest.json",mediaType:"application/json"},filesChanged:1,linesChanged:1},conditions:[{type:"WorkComplete",status:"True",reason:"Captured",message:"patch captured before work cleanup",observedGeneration:1}]}}')"

	# The completed capture Pod still has direct-workspace mounted. Delete both
	# work-side Jobs and wait for their Pods to disappear before deleting the
	# PVC; otherwise Kubernetes correctly holds the PVC in Terminating behind
	# its in-use protection finalizer.
	${kubectl[@]} delete job direct-work -n "$case_namespace" --wait=false >/dev/null
	${kubectl[@]} delete job direct-capture -n "$case_namespace" --wait=false >/dev/null
	wait_deleted job direct-work || die 'work child survived capture cleanup'
	wait_deleted job direct-capture || die 'capture child survived work cleanup'
	${kubectl[@]} delete pvc direct-workspace -n "$case_namespace" --wait=false >/dev/null
	wait_deleted pvc direct-workspace || die 'work PVC survived capture cleanup'

	verify_json=$(${kubectl[@]} get job direct-verify -n "$case_namespace" -o json)
	printf '%s\n' "$verify_json" | jq -e 'all(.spec.template.spec.volumes[]?.persistentVolumeClaim.claimName?; . != "direct-workspace")' >/dev/null || die 'verify Job mounts the work PVC'
	wait_complete direct-verify
	verify_logs=$(${kubectl[@]} logs job/direct-verify -n "$case_namespace")
	printf '%s\n' "$verify_logs" | jq -e --arg uid "$case_uid" --arg digest "$spec_digest" --arg base "$base_sha" 'select(.phase == "Verifying" and .verdict == "Accepted" and .runUid == $uid and .specDigest == $digest and .baseSHA == $base and .evidence == "fresh-verify-volume")' >/dev/null || die 'verification evidence did not prove the fresh boundary'
	verify_uid=$(get_uid job direct-verify)
	gate_uid=$(get_uid gate direct-mock-gate)
	patch_status "$(jq -cn --arg digest "$spec_digest" --arg base "$base_sha" --arg uid "$verify_uid" --arg gateuid "$gate_uid" --arg plan "$fixed_plan_digest" --arg report "$report_digest" '{status:{phase:"Succeeded",observedGeneration:1,specDigest:$digest,baseSHA:$base,verifySandboxRef:{name:"direct-verify",kind:"Job",uid:$uid,role:"verify",specDigest:$digest,planFingerprint:$plan},gate:{name:"direct-mock-gate",uid:$gateuid,generation:1,mode:"shadow",verdict:"Accepted",reportRef:{uri:"s3://offline/direct/report.json",digest:$report,kind:"verification-report",name:"report.json",mediaType:"application/json"},checks:[{name:"fresh-verify-volume",passed:true,message:"verify did not mount direct-workspace"}]},conditions:[{type:"WorkComplete",status:"True",reason:"Captured",message:"captured patch is immutable",observedGeneration:1},{type:"Verified",status:"True",reason:"GateAccepted",message:"offline verifier accepted the patch",observedGeneration:1}]}}')"
	[[ "$(${kubectl[@]} get agentrun "$case_name" -n "$case_namespace" -o jsonpath='{.status.phase}')" == Succeeded ]] || die 'terminal success status was not persisted'

	wait_failed direct-verify-failure
	expected_failure=$(printf '%s' '{"code":"VerificationCommandFailed","message":"offline verifier rejected the exact patch","retryable":false,"phase":"Verifying","runUid":"'"$case_uid"'","specDigest":"'"$spec_digest"'","baseSHA":"'"$base_sha"'"}')
	failure_logs=$(${kubectl[@]} logs job/direct-verify-failure -n "$case_namespace")
	[[ "$failure_logs" == "$expected_failure" ]] || die 'failure evidence was not exact'

	${kubectl[@]} delete agentrun "$case_name" -n "$case_namespace" --wait=false >/dev/null
	for child in direct-expected-patch direct-verify-workspace direct-capture direct-verify direct-verify-failure; do
		kind=configmap
		case "$child" in
			direct-verify-workspace) kind=pvc ;;
			direct-capture|direct-verify|direct-verify-failure) kind=job ;;
		esac
		wait_deleted "$kind" "$child" || die "$kind/$child was not garbage-collected from AgentRun ownerRef"
	done
	printf '%s\n' 'direct-live-gate: success boundary PASS'
}

run_failure_case() {
	case_namespace="${namespace_prefix}-failure-${run_id}"
	case_name=direct-failure
	case_uid=
	spec_digest=sha256:3333333333333333333333333333333333333333333333333333333333333333
	base_sha=$fixed_base_sha
	ensure_namespace_absent "$case_namespace"
	${kubectl[@]} create namespace "$case_namespace" >/dev/null
	namespaces+=("$case_namespace")
	render_fixture "$fixture" | ${kubectl[@]} apply --server-side --field-manager=direct-live-gate -f - >/dev/null
	case_uid=$(get_uid agentrun "$case_name")
	[[ -n "$case_uid" ]] || die 'failure AgentRun admission returned no UID'
	patch_status "$(jq -cn --arg digest "$spec_digest" --arg base "$base_sha" '{status:{phase:"Pending",observedGeneration:1,specDigest:$digest,baseSHA:$base,conditions:[{type:"Admitted",status:"True",reason:"FixtureAdmitted",message:"offline failure fixture admitted",observedGeneration:1}]}}')"
	render "$failure_child" | ${kubectl[@]} apply --server-side --field-manager=direct-live-gate -f - >/dev/null
	check_owner job direct-verify-failure "$case_uid"
	wait_failed direct-verify-failure
	expected_failure=$(printf '%s' '{"code":"VerificationCommandFailed","message":"offline verifier rejected the exact patch","retryable":false,"phase":"Verifying","runUid":"'"$case_uid"'","specDigest":"'"$spec_digest"'","baseSHA":"'"$base_sha"'"}')
	failure_logs=$(${kubectl[@]} logs job/direct-verify-failure -n "$case_namespace")
	[[ "$failure_logs" == "$expected_failure" ]] || die 'failure run log evidence changed'
	failure_payload=$(jq -cn --arg code VerificationCommandFailed --arg message 'offline verifier rejected the exact patch' --arg digest "$spec_digest" --arg base "$base_sha" '{status:{phase:"Failed",observedGeneration:1,specDigest:$digest,baseSHA:$base,failure:{code:$code,message:$message,retryable:false},conditions:[{type:"Verified",status:"False",reason:$code,message:$message,observedGeneration:1}]}}')
	patch_status "$failure_payload"
	status_json=$(${kubectl[@]} get agentrun "$case_name" -n "$case_namespace" -o json)
	printf '%s\n' "$status_json" | jq -e --arg digest "$spec_digest" --arg base "$base_sha" '.status.phase == "Failed" and .status.failure.code == "VerificationCommandFailed" and .status.failure.retryable == false and .status.specDigest == $digest and .status.baseSHA == $base' >/dev/null || die 'failure status did not preserve exact bounded evidence'
	${kubectl[@]} delete agentrun "$case_name" -n "$case_namespace" --wait=false >/dev/null
	wait_deleted job direct-verify-failure || die 'failure child was not garbage-collected from AgentRun ownerRef'
	printf '%s\n' 'direct-live-gate: failure evidence PASS'
}

run_success_case
run_failure_case
echo 'direct-live-gate: LIVE PASS (provider-free contract only)'
