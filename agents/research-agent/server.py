import asyncio
import json
import logging
import os
import re
import sqlite3
import uuid
from datetime import datetime, timezone
from typing import Any

import httpx
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse, StreamingResponse
from pydantic import BaseModel

IRRELEVANT_KEYWORDS = re.compile(
    r'\b(SOLID\s+principles?|object.?oriented\s+(design|programming)|'
    r'software\s+design\s+principles?|programming\s+(principles?|concepts?)|'
    r'coding\s+(principles?|best\s+practices)|JavaScript\s+(library|framework)|'
    r'TypeScript|React\s+(library|framework|js|jsx)|web\s+(development|framework))\b',
    re.IGNORECASE
)

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("research-agent")

NVIDIA_API_KEY = os.environ["NVIDIA_API_KEY"]
NIM_LEAD_MODEL = os.environ.get("NIM_LEAD_MODEL", "nvidia/llama-3.3-nemotron-super-49b-v1")
NIM_WORKER_MODEL = os.environ.get("NIM_WORKER_MODEL", "nvidia/nvidia-nemotron-nano-9b-v2")
CRW_URL = os.environ.get("CRW_URL", "http://172.17.0.1:3002")
NIM_BASE = "https://integrate.api.nvidia.com/v1"
DATA_DIR = "/data"
TASK_DB = os.path.join(DATA_DIR, "tasks.db")

CONTENT_FARM_DENYLIST = {
    "medium.com/@", "newsbreak.com", "hubpages.com", "ezinearticles.com",
    "articlesfactory.com", "selfgrowth.com", "buzzle.com", "infobarrel.com",
    "wizzley.com", "socyberty.com", "helium.com", "triond.com",
    "xomba.com", "articlebase.com", "articledashboard.com",
}
LISTICLE_PATTERN = re.compile(r'top\s*\d+|best\s*\w+\s*\d{4}|vs\.|\d+\s+ways?\s+to', re.IGNORECASE)
HIGH_SOURCE_DOMAINS = re.compile(r'\.gov$|\.edu$|arxiv\.org|github\.com')

ngrok_pattern = re.compile(r'https?://[a-f0-9]+\.ngrok\.io', re.IGNORECASE)

DB_INIT_SQL = """
CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    query TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'submitted',
    phase TEXT,
    plan TEXT,
    findings TEXT,
    draft_report TEXT,
    final_report TEXT,
    error TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
"""

def get_db():
    conn = sqlite3.connect(TASK_DB)
    conn.row_factory = sqlite3.Row
    return conn

def init_db():
    os.makedirs(DATA_DIR, exist_ok=True)
    conn = get_db()
    conn.execute(DB_INIT_SQL)
    conn.commit()
    cur = conn.execute("SELECT id, status FROM tasks WHERE status IN ('working','submitted')")
    for row in cur.fetchall():
        conn.execute("UPDATE tasks SET status='failed', error='interrupted by restart', updated_at=? WHERE id=?", (datetime.now(timezone.utc).isoformat(), row["id"]))
    conn.commit()
    conn.close()

init_db()

class TaskStore:
    @staticmethod
    def create(task_id: str, query: str):
        conn = get_db()
        now = datetime.now(timezone.utc).isoformat()
        conn.execute("INSERT INTO tasks (id, query, status, phase, created_at, updated_at) VALUES (?,?,?,?,?,?)",
                     (task_id, query, "submitted", None, now, now))
        conn.commit()
        conn.close()

    @staticmethod
    def get(task_id: str):
        conn = get_db()
        row = conn.execute("SELECT * FROM tasks WHERE id=?", (task_id,)).fetchone()
        conn.close()
        if row is None:
            return None
        d = dict(row)
        for key in ("plan", "findings"):
            if d.get(key):
                try:
                    d[key] = json.loads(d[key])
                except (json.JSONDecodeError, TypeError):
                    pass
        return d

    @staticmethod
    def update(task_id: str, **kwargs):
        conn = get_db()
        now = datetime.now(timezone.utc).isoformat()
        sets = []
        vals = []
        for k, v in kwargs.items():
            if k in ("plan", "findings") and not isinstance(v, str):
                v = json.dumps(v)
            sets.append(f"{k}=?")
            vals.append(v)
        sets.append("updated_at=?")
        vals.append(now)
        vals.append(task_id)
        conn.execute(f"UPDATE tasks SET {','.join(sets)} WHERE id=?", tuple(vals))
        conn.commit()
        conn.close()

    @staticmethod
    def list_tasks(limit=20):
        conn = get_db()
        rows = conn.execute("SELECT id, query, status, phase, created_at, updated_at FROM tasks ORDER BY created_at DESC LIMIT ?", (limit,)).fetchall()
        conn.close()
        return [dict(r) for r in rows]


class JSONRPCRequest(BaseModel):
    jsonrpc: str = "2.0"
    method: str
    params: dict | None = None
    id: str | int | None = None

class JSONRPCError(Exception):
    def __init__(self, code: int, message: str, data: Any = None):
        self.code = code
        self.message = message
        self.data = data


active_tasks: dict[str, asyncio.Task] = {}
cancel_events: dict[str, asyncio.Event] = {}


async def call_nim(model: str, messages: list[dict], tools: list | None = None, tool_choice: str | None = None, max_tokens: int = 4096, temperature: float = 0.3) -> dict:
    body = {
        "model": model,
        "messages": messages,
        "max_tokens": max_tokens,
        "temperature": temperature,
    }
    if tools:
        body["tools"] = tools
    if tool_choice:
        body["tool_choice"] = tool_choice
    async with httpx.AsyncClient(timeout=180.0) as client:
        resp = await client.post(
            f"{NIM_BASE}/chat/completions",
            headers={"Authorization": f"Bearer {NVIDIA_API_KEY}", "Content-Type": "application/json"},
            json=body,
        )
        if resp.status_code != 200:
            raise RuntimeError(f"NIM API error {resp.status_code}: {resp.text}")
        return resp.json()

def extract_tool_calls(nim_resp: dict) -> list[dict]:
    msg = nim_resp["choices"][0]["message"]
    tcs = msg.get("tool_calls")
    if not tcs:
        return []
    return [{"id": tc["id"], "type": tc["type"], "function": {"name": tc["function"]["name"], "arguments": json.loads(tc["function"]["arguments"])}} for tc in tcs]

def extract_content(nim_resp: dict) -> str:
    return nim_resp["choices"][0]["message"].get("content", "") or ""


SEARXNG_URL = "http://searxng:8080"

async def crw_search(query: str, recency_days: int | None = None) -> list[dict]:
    try:
        q = query.replace("-", " ")
        async with httpx.AsyncClient(timeout=30.0) as client:
            resp = await client.get(
                f"{SEARXNG_URL}/search",
                params={"q": q, "format": "json", "language": "en"},
                headers={"User-Agent": "Mozilla/5.0", "Accept": "application/json"},
            )
            if resp.status_code != 200:
                logger.warning(f"searxng_search returned {resp.status_code}")
                return []
            data = resp.json()
            results = data.get("results", [])
            out = []
            seen = set()
            for r in results:
                url = r.get("url", "")
                if url and url not in seen:
                    seen.add(url)
                    out.append({
                        "title": r.get("title", ""),
                        "url": url,
                        "snippet": r.get("content", ""),
                        "published_date": r.get("publishedDate", None),
                    })
            return out
    except Exception as e:
        logger.warning(f"crw_search error: {e}")
        return []

async def crw_scrape(url: str) -> str:
    async with httpx.AsyncClient(timeout=60.0) as client:
        try:
            resp = await client.post(f"{CRW_URL}/v1/scrape", json={"url": url, "formats": ["markdown"], "onlyMainContent": True})
            if resp.status_code == 200:
                data = resp.json()
                if isinstance(data, dict):
                    return data.get("data", {}).get("markdown", data.get("markdown", "")) or json.dumps(data)
                return str(data)
            logger.warning(f"crw_scrape returned {resp.status_code} for {url}")
            return ""
        except Exception as e:
            logger.warning(f"crw_scrape error for {url}: {e}")
            return ""


def score_source(url: str, title: str = "", snippet: str = "", published_date: str | None = None, recency_days: int | None = None, query_context: str = "") -> float:
    score = 5.0
    reasons = []

    url_lower = url.lower()
    title_lower = title.lower()
    snippet_lower = snippet.lower()

    is_battery_query = any(kw in query_context.lower() for kw in ["battery", "batteries", "energy storage", "ev", "electric vehicle"])
    if is_battery_query:
        battery_terms = re.compile(r'\b(battery|batteries|energy.?storage|electrolyte|EV|electric.?vehicle)\b', re.IGNORECASE)
        has_battery = battery_terms.search(title_lower) or battery_terms.search(snippet_lower) or bool(url_lower.count("battery"))
        if not has_battery:
            score -= 3.0
            reasons.append("no battery-related terms")

    if IRRELEVANT_KEYWORDS.search(title_lower) or IRRELEVANT_KEYWORDS.search(snippet_lower):
        score -= 4.0
        reasons.append("irrelevant topic (SOLID/software/programming)")

    if any(domain in url_lower for domain in CONTENT_FARM_DENYLIST):
        score -= 3.0
        reasons.append("content-farm domain")
    if ngrok_pattern.search(url):
        score -= 3.0
        reasons.append("ngrok tunnel")
    if LISTICLE_PATTERN.search(title_lower) or LISTICLE_PATTERN.search(snippet_lower):
        score -= 2.0
        reasons.append("listicle/top-N/best-of pattern")

    if HIGH_SOURCE_DOMAINS.search(url_lower):
        score += 2.0
        reasons.append("high-authority domain")
    if "wikipedia.org" in url_lower:
        score += 1.5
        reasons.append("wikipedia")
    if any(domain in url_lower for domain in ["techcrunch.com", "reuters.com", "apnews.com", "bloomberg.com", "nature.com", "ieee.org", "acm.org", "sciencedirect.com", "arxiv.org"]):
        score += 1.0
        reasons.append("reputable source")

    if published_date and recency_days:
        try:
            pub = datetime.fromisoformat(published_date.replace("Z", "+00:00"))
            now = datetime.now(timezone.utc)
            age_days = (now - pub).days
            if age_days <= recency_days:
                score += 1.5
                reasons.append("recent")
            elif age_days <= recency_days * 2:
                score += 0.5
                reasons.append("moderately recent")
            else:
                score -= 1.0
                reasons.append("stale")
        except (ValueError, TypeError):
            pass

    score = max(0.0, min(10.0, score))
    return round(score, 2), "; ".join(reasons)


async def call_nim_with_retry(model: str, messages: list[dict], tools: list | None = None, tool_choice: str | None = None, max_tokens: int = 4096, temperature: float = 0.3, retries: int = 3) -> dict:
    last_err = None
    for attempt in range(retries):
        try:
            return await call_nim(model, messages, tools, tool_choice, max_tokens, temperature)
        except Exception as e:
            last_err = e
            logger.warning(f"NIM call attempt {attempt+1} failed: {e}")
            await asyncio.sleep(2 ** attempt)
    raise RuntimeError(f"NIM call failed after {retries} retries: {last_err}")


SYSTEM_PLAN = """You are a research planning agent. Given a user query, decompose it into 3-5 sub-questions that each target a distinct aspect of the topic. For each sub-question, provide search hints (2-3 keywords/phrases) that will help find relevant information. Output JSON: {"sub_questions": [{"id": "sq1", "question": "...", "search_hints": ["hint1", "hint2"]}, ...]}"""

SYSTEM_RESEARCH = """You are a research extraction agent. Given search results and scraped content, extract key facts, relevant URLs, and publication dates. For each fact, note which source(s) support it. Output JSON: {"findings": [{"fact": "...", "sources": [{"url": "...", "date": "..."}], "corroborating_count": 2}], "resolved": true/false, "refined_query": "..."}. Set resolved=true only if at least 2 independent sources (different domains) corroborate key facts."""

SYSTEM_SYNTHESIZE = """You are a research synthesis agent. Given research findings across multiple sub-questions, synthesize them into a comprehensive markdown report. Use [1], [2], etc. as citation markers. Include a Summary section, per-sub-question findings, a Sources section, and a Caveats section."""


async def run_plan_phase(query: str, cancel: asyncio.Event) -> dict:
    messages = [
        {"role": "system", "content": SYSTEM_PLAN},
        {"role": "user", "content": f"Query: {query}"}
    ]
    resp = await call_nim_with_retry(NIM_LEAD_MODEL, messages, max_tokens=2048, temperature=0.3)
    content = extract_content(resp)
    try:
        plan = json.loads(content)
    except json.JSONDecodeError:
        match = re.search(r'\{.*\}', content, re.DOTALL)
        if match:
            try:
                plan = json.loads(match.group())
            except json.JSONDecodeError:
                plan = {"sub_questions": [{"id": "sq1", "question": query, "search_hints": [query]}]}
        else:
            plan = {"sub_questions": [{"id": "sq1", "question": query, "search_hints": [query]}]}
    return plan


async def worker_research(sub_question: dict, cancel: asyncio.Event, user_query: str = "") -> dict:
    sq_id = sub_question["id"]
    question = sub_question.get("question", user_query)
    hints = sub_question.get("search_hints", [])

    base_queries = hints[:] if hints else [question]

    for iteration in range(1, 5):
        if cancel.is_set():
            return {"sub_question_id": sq_id, "resolved": False, "findings": [], "sources": [], "cancelled": True}

        idx = min(iteration - 1, len(base_queries) - 1)
        q = base_queries[idx]
        results = await crw_search(q, recency_days=365)
        if not results:
            continue

        scored = []
        for r in results:
            s, reason = score_source(
                r.get("url", ""),
                r.get("title", ""),
                r.get("snippet", ""),
                r.get("published_date"),
                recency_days=365,
                query_context=q,
            )
            scored.append((s, reason, r))

        scored.sort(key=lambda x: x[0], reverse=True)
        logger.info(f"[{sq_id}] iteration {iteration}: {len(scored)} results scored")

        excluded = [(r, s, reason) for s, reason, r in scored if s < 4.0]
        for r, s, reason in excluded:
            logger.info(f"[{sq_id}] EXCLUDED score={s}: {r.get('url','')} ({reason})")

        top_scored = [item for item in scored if item[0] >= 4.0][:3]
        if not top_scored and scored:
            top_scored = scored[:3]

        scraped = []
        for s, reason, r in top_scored:
            if cancel.is_set():
                return {"sub_question_id": sq_id, "resolved": False, "findings": [], "sources": [], "cancelled": True}
            md = await crw_scrape(r["url"])
            scraped.append({"url": r["url"], "title": r.get("title", ""), "markdown": md[:8000], "published_date": r.get("published_date")})

        context = json.dumps([{
            "url": s["url"],
            "title": s["title"],
            "content_preview": s["markdown"][:2000],
            "date": s["published_date"],
        } for s in scraped], indent=2)

        messages = [
            {"role": "system", "content": SYSTEM_RESEARCH},
            {"role": "user", "content": f"Sub-question: {question}\nSearch hints: {hints}\n\nScraped content:\n{context}\n\nExtract findings and determine if sufficiently corroborated (>=2 independent sources from different domains)."}
        ]
        resp = await call_nim_with_retry(NIM_WORKER_MODEL, messages, max_tokens=2048, temperature=0.2)
        content = extract_content(resp)

        try:
            result = json.loads(content)
        except json.JSONDecodeError:
            match = re.search(r'\{.*\}', content, re.DOTALL)
            if match:
                try:
                    result = json.loads(match.group())
                except json.JSONDecodeError:
                    result = {"findings": [{"fact": content[:500], "sources": [], "corroborating_count": 0}], "resolved": False, "refined_query": question}
            else:
                result = {"findings": [{"fact": content[:500], "sources": [], "corroborating_count": 0}], "resolved": False, "refined_query": question}

        sources = [{"url": r["url"], "title": r["title"], "date": r["published_date"], "scored": s} for s, reason, r in top_scored]
        resolved = result.get("resolved", False)
        refined = result.get("refined_query", "")

        if resolved or result.get("corroborating_count", 0) >= 2:
            logger.info(f"[{sq_id}] RESOLVED at iteration {iteration} with {result.get('corroborating_count', 0)} corroborating sources")
            return {"sub_question_id": sq_id, "resolved": True, "findings": result.get("findings", []), "sources": sources, "iteration": iteration}

        if refined and iteration < 4:
            base_query = f"\"{refined}\""
            logger.info(f"[{sq_id}] refining query: {refined}")

    return {"sub_question_id": sq_id, "resolved": False, "findings": [], "sources": sources if 'sources' in dir() else [], "iteration": 4}


async def run_research_phase(plan: dict, task_id: str, cancel: asyncio.Event, sse_queue: asyncio.Queue | None = None, user_query: str = "") -> list[dict]:
    sub_questions = plan.get("sub_questions", [])
    logger.info(f"Starting research phase with {len(sub_questions)} sub-questions")

    async def worker_wrapper(sq):
        result = await worker_research(sq, cancel, user_query)
        if sse_queue:
            await sse_queue.put({"type": "worker_complete", "data": result})
        return result

    results = await asyncio.gather(*[worker_wrapper(sq) for sq in sub_questions])
    TaskStore.update(task_id, findings=results, phase="research_complete")
    return results


async def run_synthesize_phase(findings: list[dict], query: str, cancel: asyncio.Event) -> str:
    findings_json = json.dumps(findings, indent=2)
    messages = [
        {"role": "system", "content": SYSTEM_SYNTHESIZE},
        {"role": "user", "content": f"Query: {query}\n\nFindings:\n{findings_json}\n\nProduce a comprehensive markdown report with Summary, per-sub-question findings with [n] citations, Sources section, and Caveats section."}
    ]
    resp = await call_nim_with_retry(NIM_LEAD_MODEL, messages, max_tokens=8192, temperature=0.3)
    return extract_content(resp)


async def run_deep_research(task_id: str, query: str, sse_queue: asyncio.Queue | None = None):
    cancel = cancel_events.get(task_id, asyncio.Event())
    if cancel.is_set():
        return

    try:
        TaskStore.update(task_id, status="working", phase="planning")
        if sse_queue:
            await sse_queue.put({"type": "phase", "phase": "planning"})
        logger.info(f"[{task_id}] Phase 1: Planning")
        plan = await run_plan_phase(query, cancel)
        if cancel.is_set():
            TaskStore.update(task_id, status="canceled", phase=None)
            return
        TaskStore.update(task_id, phase="planning", plan=plan)
        logger.info(f"[{task_id}] Plan: {json.dumps(plan, indent=2)}")

        if sse_queue:
            await sse_queue.put({"type": "phase", "phase": "researching", "plan": plan})
        TaskStore.update(task_id, phase="researching")
        logger.info(f"[{task_id}] Phase 2: Research")
        findings = await run_research_phase(plan, task_id, cancel, sse_queue, query)
        if cancel.is_set():
            TaskStore.update(task_id, status="canceled", phase=None)
            return

        if sse_queue:
            await sse_queue.put({"type": "phase", "phase": "synthesizing"})
        TaskStore.update(task_id, phase="synthesizing")
        logger.info(f"[{task_id}] Phase 3: Synthesize")
        draft_report = await run_synthesize_phase(findings, query, cancel)
        if cancel.is_set():
            TaskStore.update(task_id, status="canceled", phase=None)
            return
        TaskStore.update(task_id, status="completed", phase="completed", final_report=draft_report)
        logger.info(f"[{task_id}] Final report generated ({len(draft_report)} chars)")

        if sse_queue:
            await sse_queue.put({"type": "phase", "phase": "completed"})
            await sse_queue.put({"type": "result", "report": draft_report})

    except Exception as e:
        logger.exception(f"[{task_id}] Error: {e}")
        TaskStore.update(task_id, status="failed", phase=None, error=str(e))
        if sse_queue:
            await sse_queue.put({"type": "error", "error": str(e)})
    finally:
        if task_id in active_tasks:
            del active_tasks[task_id]
        if task_id in cancel_events:
            del cancel_events[task_id]


app = FastAPI(title="Research Agent")


@app.get("/.well-known/agent-card.json")
async def agent_card(request: Request):
    return {
        "name": "Deep Research Agent",
        "description": "Multi-phase deep research agent: plans sub-questions, researches them in parallel with source-quality filtering and early stopping, synthesizes findings, and verifies citations against source content before returning a report.",
        "url": "https://agents.astatide.com/agents/research-agent/a2a",
        "version": "1.0.0",
        "capabilities": {"streaming": True},
        "skills": [{
            "id": "deep-research",
            "name": "Deep Research",
            "description": "Conduct in-depth research using parallel sub-agent investigation, source quality filtering, and citation verification. Returns a sourced markdown report.",
            "tags": ["research", "web", "deep-research"]
        }]
    }


def make_jsonrpc_error(code: int, message: str, data: Any = None, req_id: Any = None) -> JSONResponse:
    return JSONResponse(
        status_code=200,
        content={"jsonrpc": "2.0", "error": {"code": code, "message": message, "data": data}, "id": req_id}
    )


@app.post("/a2a")
async def a2a_handler(request: Request):
    body = await request.json()
    req_id = body.get("id")
    method = body.get("method")
    params = body.get("params", {})

    if method == "tasks/send":
        query = params.get("query", "")
        if not query:
            return make_jsonrpc_error(-32602, "Missing query", req_id=req_id)
        task_id = str(uuid.uuid4())
        TaskStore.create(task_id, query)
        cancel_events[task_id] = asyncio.Event()
        active_tasks[task_id] = asyncio.create_task(run_deep_research(task_id, query))
        return JSONResponse(content={"jsonrpc": "2.0", "result": {"id": task_id, "status": "submitted"}, "id": req_id})

    elif method == "tasks/get":
        task_id = params.get("id", "")
        task = TaskStore.get(task_id)
        if not task:
            return make_jsonrpc_error(-32000, "Task not found", req_id=req_id)
        return JSONResponse(content={"jsonrpc": "2.0", "result": task, "id": req_id})

    elif method == "tasks/cancel":
        task_id = params.get("id", "")
        if task_id in cancel_events:
            cancel_events[task_id].set()
        task = TaskStore.get(task_id)
        if task and task["status"] in ("submitted", "working"):
            TaskStore.update(task_id, status="canceled", phase=None)
        return JSONResponse(content={"jsonrpc": "2.0", "result": {"id": task_id, "status": "canceled"}, "id": req_id})

    elif method == "tasks/sendSubscribe":
        query = params.get("query", "")
        if not query:
            return make_jsonrpc_error(-32602, "Missing query", req_id=req_id)
        task_id = str(uuid.uuid4())
        TaskStore.create(task_id, query)

        sse_queue: asyncio.Queue = asyncio.Queue()
        cancel_events[task_id] = asyncio.Event()

        async def event_generator():
            try:
                yield f"event: task_started\ndata: {json.dumps({'id': task_id, 'status': 'submitted'})}\n\n"
                research_task = asyncio.create_task(
                    run_deep_research(task_id, query, sse_queue)
                )
                active_tasks[task_id] = research_task

                while True:
                    try:
                        msg = await asyncio.wait_for(sse_queue.get(), timeout=1.0)
                    except asyncio.TimeoutError:
                        if research_task.done():
                            break
                        continue

                    if msg["type"] == "phase":
                        yield f"event: phase\ndata: {json.dumps({'phase': msg['phase'], 'plan': msg.get('plan')})}\n\n"
                    elif msg["type"] == "worker_complete":
                        yield f"event: worker_complete\ndata: {json.dumps(msg['data'])}\n\n"
                    elif msg["type"] == "result":
                        yield f"event: result\ndata: {json.dumps(msg)}\n\n"
                    elif msg["type"] == "error":
                        yield f"event: error\ndata: {json.dumps(msg)}\n\n"

                task = TaskStore.get(task_id)
                yield f"event: task_complete\ndata: {json.dumps(task)}\n\n"
            except asyncio.CancelledError:
                pass
            finally:
                if task_id in cancel_events:
                    cancel_events[task_id].set()

        return StreamingResponse(event_generator(), media_type="text/event-stream")

    else:
        return make_jsonrpc_error(-32601, f"Method not found: {method}", req_id=req_id)


@app.get("/health")
async def health():
    return {"status": "ok"}

@app.get("/tasks")
async def list_tasks(limit: int = 20):
    return TaskStore.list_tasks(limit)
