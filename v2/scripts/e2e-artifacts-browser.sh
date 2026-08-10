#!/bin/sh
set -eu

repository_dir=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
web_dir="$repository_dir/v2/web"
browser_port=${AGW_BROWSER_PORT:-4173}
base_url="http://127.0.0.1:$browser_port"
browser_session="agw-artifacts-e2e-$$"
vite_pid=
remove_results=0

if [ -n "${AGW_BROWSER_E2E_OUTPUT_DIR:-}" ]; then
  results_dir=$AGW_BROWSER_E2E_OUTPUT_DIR
  mkdir -p -- "$results_dir"
else
  results_dir=$(mktemp -d "${TMPDIR:-/tmp}/agw-browser-e2e.XXXXXX")
  remove_results=1
fi

export AGENT_BROWSER_SESSION=$browser_session
export AGENT_BROWSER_ALLOWED_DOMAINS=127.0.0.1
export AGENT_BROWSER_SCREENSHOT_DIR=$results_dir

if [ "${AGW_BROWSER_NO_SANDBOX:-0}" = "1" ]; then
  export AGENT_BROWSER_ARGS=--no-sandbox
fi

browser() {
  npx --yes agent-browser@latest "$@"
}

cleanup() {
  exit_code=$?
  trap - EXIT INT TERM
  browser close >/dev/null 2>&1 || true
  if [ -n "$vite_pid" ]; then
    kill "$vite_pid" >/dev/null 2>&1 || true
    wait "$vite_pid" >/dev/null 2>&1 || true
  fi
  if [ "$exit_code" -ne 0 ]; then
    echo "Artifact browser E2E failed. Vite output follows:" >&2
    sed -n '1,240p' "$results_dir/vite.log" >&2 || true
    echo "Browser evidence: $results_dir" >&2
  elif [ "$remove_results" -eq 1 ]; then
    case "$results_dir" in
      "${TMPDIR:-/tmp}"/agw-browser-e2e.*) rm -r -- "$results_dir" ;;
    esac
  fi
  exit "$exit_code"
}
trap cleanup EXIT INT TERM

assert_contains() {
  value=$1
  expected=$2
  description=$3
  case "$value" in
    *"$expected"*) ;;
    *)
      echo "Expected $description to contain: $expected" >&2
      exit 1
      ;;
  esac
}

(
  cd "$web_dir"
  VITE_AGW_MODE=demo npm run dev -- --host 127.0.0.1 --port "$browser_port" --strictPort
) >"$results_dir/vite.log" 2>&1 &
vite_pid=$!

ready=0
attempt=0
while [ "$attempt" -lt 60 ]; do
  if curl --silent --fail --output /dev/null "$base_url/"; then
    ready=1
    break
  fi
  if ! kill -0 "$vite_pid" 2>/dev/null; then
    break
  fi
  attempt=$((attempt + 1))
  sleep 1
done
if [ "$ready" -ne 1 ]; then
  echo "Vite did not become ready at $base_url" >&2
  exit 1
fi

browser open "$base_url/#artifacts"
browser set viewport 1440 1000
browser wait 400

page_text=$(browser get text body)
assert_contains "$page_text" "release-notes.html" "artifact library"
assert_contains "$page_text" "Version 2" "current artifact version"
browser screenshot "$results_dir/artifacts-desktop.png"

browser select 'select[aria-label="Artifact version"]' demo-release-v1
browser click '.artifact-tabs [role="tab"]:nth-child(2)'
source_text=$(browser get text '[role="tabpanel"]')
assert_contains "$source_text" "Initial release summary." "version 1 source"

browser click '.artifact-library-item:nth-child(2)'
browser click '.artifact-tabs [role="tab"]:first-child'
markdown_text=$(browser get text '[role="tabpanel"]')
assert_contains "$markdown_text" "Review summary" "Markdown preview"
assert_contains "$markdown_text" "All required checks passed" "Markdown content"
browser eval "(() => { const panel = document.querySelector('[role=tabpanel]'); if (!panel) throw new Error('missing artifact panel'); if (panel.querySelectorAll('a').length !== 0) throw new Error('Markdown links must remain inert'); if (panel.querySelectorAll('script').length !== 0) throw new Error('Markdown scripts must be removed'); return 'Markdown security verified'; })()"
browser screenshot "$results_dir/artifacts-markdown.png"

browser download '.artifact-workspace .secondary-button' "$results_dir/review-summary.md"
test -s "$results_dir/review-summary.md"
downloaded=$(sed -n '1,40p' "$results_dir/review-summary.md")
assert_contains "$downloaded" "# Review summary" "downloaded Markdown artifact"

browser click '.artifact-library-item:first-child'
browser click '.artifact-tabs [role="tab"]:first-child'
browser wait 'iframe.artifact-preview-frame'
browser eval "(() => { const frame = document.querySelector('iframe.artifact-preview-frame'); if (!frame) throw new Error('missing preview iframe'); const sandbox = frame.getAttribute('sandbox'); const srcdoc = frame.getAttribute('srcdoc') || ''; if (sandbox !== '') throw new Error('static preview iframe must use an empty sandbox policy'); if (sandbox.includes('allow-same-origin')) throw new Error('preview iframe must not have same-origin privileges'); if (!srcdoc.includes(\"connect-src 'none'\")) throw new Error('preview CSP must deny network connections'); return 'iframe security verified'; })()"

accessibility=$(browser a11y --tags wcag2a,wcag2aa --json)
printf '%s\n' "$accessibility" >"$results_dir/accessibility.json"
assert_contains "$accessibility" '"violations":[]' "WCAG A/AA accessibility result"

browser set viewport 390 844
browser screenshot "$results_dir/artifacts-mobile.png"
mobile_text=$(browser get text body)
assert_contains "$mobile_text" "release-notes.html" "mobile artifact library"
assert_contains "$mobile_text" "Download" "mobile artifact controls"

errors=$(browser errors --json)
printf '%s\n' "$errors" >"$results_dir/browser-errors.json"
assert_contains "$errors" '"errors":[]' "browser runtime error report"

echo "Artifact browser E2E passed (desktop, mobile, download, sandbox, Markdown, accessibility)."
