import asyncio
import hashlib
import json
import logging
import os
import secrets
import uuid
from datetime import datetime, timedelta, timezone
from typing import Any
from urllib.parse import urlencode

import base64
import httpx
import jwt
import yaml
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import HTMLResponse, JSONResponse, RedirectResponse, Response, StreamingResponse
from jwt import PyJWKClient
from pydantic import BaseModel

logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("agent-gateway")

GATEWAY_PORT = 8092
PUBLIC_HOST = os.environ.get("PUBLIC_HOST", "https://agents.astatide.com")
CLOUDFLARE_TEAM_DOMAIN = os.environ.get("CLOUDFLARE_TEAM_DOMAIN", "")
CLOUDFLARE_AUD = os.environ.get("CLOUDFLARE_AUD", "")
LOCAL_TOKEN_SECRET = os.environ.get("LOCAL_TOKEN_SECRET", "")

# --- RSA key pair for OAuth token signing ---
_private_key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
_public_key = _private_key.public_key()

_private_pem = _private_key.private_bytes(
    encoding=serialization.Encoding.PEM,
    format=serialization.PrivateFormat.PKCS8,
    encryption_algorithm=serialization.NoEncryption(),
)
_public_pem = _public_key.public_bytes(
    encoding=serialization.Encoding.PEM,
    format=serialization.PublicFormat.SubjectPublicKeyInfo,
)
_kid = "gateway-oauth-v1"

# --- OAuth in-memory stores ---
_auth_codes: dict[str, dict] = {}
_access_tokens: dict[str, dict] = {}
_refresh_tokens: dict[str, dict] = {}
OAUTH_CLIENTS: dict[str, dict] = {}

DEFAULT_CLIENT: dict = {"redirect_uris": []}
ACCESS_TOKEN_EXPIRY = timedelta(hours=1)
REFRESH_TOKEN_EXPIRY = timedelta(days=30)
AUTH_CODE_EXPIRY = timedelta(minutes=5)


# --- Cloudflare Access JWKS ---
_jwks_client = None

def get_jwks_client():
    global _jwks_client
    if _jwks_client is None and CLOUDFLARE_TEAM_DOMAIN:
        _jwks_client = PyJWKClient(f"https://{CLOUDFLARE_TEAM_DOMAIN}/cdn-cgi/access/certs")
    return _jwks_client


# --- Token helpers ---

def _issue_access_token(client_id: str, user_email: str | None = None, scopes: list[str] | None = None) -> tuple[str, str]:
    now = datetime.now(timezone.utc)
    jti = secrets.token_urlsafe(16)
    payload = {
        "iss": PUBLIC_HOST,
        "sub": client_id,
        "aud": PUBLIC_HOST,
        "iat": now,
        "exp": now + ACCESS_TOKEN_EXPIRY,
        "scopes": scopes or [],
        "jti": jti,
        "token_type": "access",
        "client_id": client_id,
    }
    if user_email:
        payload["email"] = user_email
    headers = {"kid": _kid, "typ": "JWT", "alg": "RS256"}
    token = jwt.encode(payload, _private_pem, algorithm="RS256", headers=headers)
    _access_tokens[jti] = {"client_id": client_id, "expires": payload["exp"]}
    return token, jti


def _issue_refresh_token(client_id: str) -> str:
    now = datetime.now(timezone.utc)
    token = secrets.token_urlsafe(48)
    _refresh_tokens[token] = {
        "client_id": client_id,
        "expires": now + REFRESH_TOKEN_EXPIRY,
        "created": now,
    }
    return token


def _get_or_create_client(client_id: str) -> dict:
    if client_id not in OAUTH_CLIENTS:
        OAUTH_CLIENTS[client_id] = dict(DEFAULT_CLIENT)
    return OAUTH_CLIENTS[client_id]


def _validate_cf_jwt(token: str) -> dict | None:
    if not CLOUDFLARE_TEAM_DOMAIN:
        return None
    try:
        jwks_client = get_jwks_client()
        if not jwks_client:
            return None
        signing_key = jwks_client.get_signing_key_from_jwt(token)
        claims = jwt.decode(
            token, signing_key.key, algorithms=["RS256"],
            audience=CLOUDFLARE_AUD,
            issuer=f"https://{CLOUDFLARE_TEAM_DOMAIN}",
        )
        return claims
    except jwt.PyJWTError as e:
        logger.warning(f"CF JWT validation failed: {e}")
        return None


def _jwks_dict() -> dict:
    nums = _public_key.public_numbers()
    n, e = nums.n, nums.e

    def _b64(num: int) -> str:
        num_bytes = num.to_bytes((num.bit_length() + 7) // 8, byteorder="big")
        return base64.urlsafe_b64encode(num_bytes).rstrip(b"=").decode()

    return {"keys": [{"kty": "RSA", "use": "sig", "alg": "RS256", "kid": _kid, "n": _b64(n), "e": _b64(e)}]}


def _verify_pkce(code_verifier: str, code_challenge: str, method: str) -> bool:
    if method == "S256":
        expected = base64.urlsafe_b64encode(hashlib.sha256(code_verifier.encode()).digest()).rstrip(b"=").decode()
        return expected == code_challenge
    return code_verifier == code_challenge


def is_docker_container(host: str) -> bool:
    parts = host.split(".")
    if len(parts) != 4:
        return False
    if parts[0] == "172" and parts[1] == "20":
        return parts[3] != "1"
    return False


app = FastAPI(title="Agent Gateway")


# --- OAuth Endpoints ---

@app.get("/authorize")
async def authorize_endpoint(request: Request):
    client_id = request.query_params.get("client_id", "")
    redirect_uri = request.query_params.get("redirect_uri", "")
    state = request.query_params.get("state", "")
    code_challenge = request.query_params.get("code_challenge", "")
    code_challenge_method = request.query_params.get("code_challenge_method", "S256")

    if not client_id or not redirect_uri:
        return HTMLResponse("Missing client_id or redirect_uri", status_code=400)

    _get_or_create_client(client_id)

    cf_jwt = request.headers.get("Cf-Access-Jwt-Assertion", "") or request.cookies.get("CF_Authorization", "")

    user_email = None
    if cf_jwt:
        claims = _validate_cf_jwt(cf_jwt)
        if claims:
            user_email = claims.get("email", claims.get("sub", "unknown"))
        else:
            return JSONResponse(status_code=401, content={"error": "invalid_token", "error_description": "Invalid Cloudflare Access token"})
    elif LOCAL_TOKEN_SECRET and request.query_params.get("local_token_secret", "") == LOCAL_TOKEN_SECRET:
        user_email = "local@localhost"
    else:
        return JSONResponse(status_code=401, content={"error": "authentication_required", "error_description": "No valid authentication"})

    code = secrets.token_urlsafe(32)
    _auth_codes[code] = {
        "client_id": client_id,
        "redirect_uri": redirect_uri,
        "code_challenge": code_challenge,
        "code_challenge_method": code_challenge_method if code_challenge else "",
        "expires": datetime.now(timezone.utc) + AUTH_CODE_EXPIRY,
        "user_email": user_email,
    }

    params = {"code": code}
    if state:
        params["state"] = state
    redirect_target = f"{redirect_uri}?{urlencode(params)}"
    return RedirectResponse(url=redirect_target, status_code=302)


@app.post("/register")
async def register_client(request: Request):
    try:
        body = await request.json()
    except Exception:
        return JSONResponse(status_code=400, content={"error": "invalid_client_metadata"})

    client_id = secrets.token_urlsafe(16)
    redirect_uris = body.get("redirect_uris", [])
    client_name = body.get("client_name", "")
    token_endpoint_auth_method = body.get("token_endpoint_auth_method", "none")
    scope = body.get("scope", "agents:read agents:write")

    OAUTH_CLIENTS[client_id] = {
        "redirect_uris": redirect_uris,
        "client_name": client_name,
        "token_endpoint_auth_method": token_endpoint_auth_method,
        "scope": scope,
        "grant_types": ["authorization_code", "refresh_token"],
    }

    return {
        "client_id": client_id,
        "client_secret": "",
        "client_id_issued_at": int(datetime.now(timezone.utc).timestamp()),
        "redirect_uris": redirect_uris,
        "grant_types": ["authorization_code", "refresh_token"],
        "response_types": ["code"],
        "token_endpoint_auth_method": token_endpoint_auth_method,
        "scope": scope,
    }


@app.post("/token")
async def token_endpoint(request: Request):
    content_type = request.headers.get("content-type", "")
    if "application/json" in content_type:
        body = await request.json() if request.body else {}
    else:
        body = await request.form() if request.body else {}

    grant_type = body.get("grant_type", "")
    client_id = body.get("client_id", "")
    client_secret = body.get("client_secret", "")

    # Basic auth extraction for client_id
    auth_header = request.headers.get("authorization", "")
    if auth_header.startswith("Basic "):
        try:
            decoded = base64.b64decode(auth_header[6:]).decode()
            if ":" in decoded:
                client_id = decoded.split(":")[0]
                if not client_secret:
                    client_secret = decoded.split(":", 1)[1]
        except Exception:
            pass

    if not client_id:
        return JSONResponse(status_code=400, content={"error": "invalid_client", "error_description": "Missing client_id"})

    _get_or_create_client(client_id)

    if grant_type == "authorization_code":
        code = body.get("code", "")
        redirect_uri = body.get("redirect_uri", "")
        code_verifier = body.get("code_verifier", "")

        stored = _auth_codes.get(code)
        if not stored:
            return JSONResponse(status_code=400, content={"error": "invalid_grant", "error_description": "Invalid authorization code"})

        if stored["client_id"] != client_id:
            del _auth_codes[code]
            return JSONResponse(status_code=400, content={"error": "invalid_grant", "error_description": "Client ID mismatch"})

        if stored["expires"] < datetime.now(timezone.utc):
            del _auth_codes[code]
            return JSONResponse(status_code=400, content={"error": "invalid_grant", "error_description": "Authorization code expired"})

        if stored.get("code_challenge"):
            if not code_verifier:
                del _auth_codes[code]
                return JSONResponse(status_code=400, content={"error": "invalid_grant", "error_description": "PKCE code_verifier required"})
            if not _verify_pkce(code_verifier, stored["code_challenge"], stored.get("code_challenge_method", "S256")):
                del _auth_codes[code]
                return JSONResponse(status_code=400, content={"error": "invalid_grant", "error_description": "PKCE verification failed"})

        user_email = stored.get("user_email")
        del _auth_codes[code]

        access_token, jti = _issue_access_token(client_id, user_email)
        refresh_token = _issue_refresh_token(client_id)

        return {
            "access_token": access_token,
            "token_type": "Bearer",
            "expires_in": int(ACCESS_TOKEN_EXPIRY.total_seconds()),
            "refresh_token": refresh_token,
            "scope": "agents:read agents:write",
            "resource": f"{PUBLIC_HOST}/mcp",
        }

    elif grant_type == "refresh_token":
        refresh_token_value = body.get("refresh_token", "")
        stored = _refresh_tokens.get(refresh_token_value)

        if not stored:
            return JSONResponse(status_code=400, content={"error": "invalid_grant", "error_description": "Invalid refresh token"})

        if stored["client_id"] != client_id:
            return JSONResponse(status_code=400, content={"error": "invalid_grant", "error_description": "Client ID mismatch"})

        if stored["expires"] < datetime.now(timezone.utc):
            del _refresh_tokens[refresh_token_value]
            return JSONResponse(status_code=400, content={"error": "invalid_grant", "error_description": "Refresh token expired"})

        del _refresh_tokens[refresh_token_value]

        access_token, jti = _issue_access_token(client_id)
        new_refresh_token = _issue_refresh_token(client_id)

        return {
            "access_token": access_token,
            "token_type": "Bearer",
            "expires_in": int(ACCESS_TOKEN_EXPIRY.total_seconds()),
            "refresh_token": new_refresh_token,
            "scope": "agents:read agents:write",
            "resource": f"{PUBLIC_HOST}/mcp",
        }

    return JSONResponse(status_code=400, content={"error": "unsupported_grant_type"})


# --- Middleware ---

@app.middleware("http")
async def auth_middleware(request: Request, call_next):
    paths_public = ("/health",)
    if request.url.path in paths_public or request.url.path.startswith("/.well-known") or request.url.path in ("/authorize", "/token", "/register"):
        return await call_next(request)

    client_host = request.client.host if request.client else ""
    if is_docker_container(client_host) or client_host in ("127.0.0.1", "::1", "localhost"):
        return await call_next(request)

    request.state.authenticated = False

    mcp_paths = ("/mcp", "/")
    is_mcp = request.method == "POST" and request.url.path in mcp_paths

    auth_header = request.headers.get("Authorization", "")
    cookie_token = request.cookies.get("CF_Authorization", "")
    cf_header = request.headers.get("Cf-Access-Jwt-Assertion", "")

    token = ""
    if auth_header.startswith("Bearer "):
        token = auth_header[7:]
    elif cf_header:
        token = cf_header
    elif cookie_token:
        token = cookie_token

    if token:
        try:
            claims = jwt.decode(
                token, _public_pem, algorithms=["RS256"],
                audience=PUBLIC_HOST,
                issuer=PUBLIC_HOST,
                options={"verify_exp": True},
            )
            request.state.user = claims.get("email", claims.get("sub", "unknown"))
            request.state.authenticated = True
            request.state.token_type = "oauth"
            return await call_next(request)
        except jwt.PyJWTError:
            pass

        if CLOUDFLARE_TEAM_DOMAIN:
            cf_claims = _validate_cf_jwt(token)
            if cf_claims:
                request.state.user = cf_claims.get("email", cf_claims.get("sub", "unknown"))
                request.state.authenticated = True
                request.state.token_type = "cloudflare"
                return await call_next(request)

    if is_mcp:
        return await call_next(request)

    return JSONResponse(status_code=401, content={"error": "Missing authorization"}, headers={
        "WWW-Authenticate": f'Bearer realm="mcp", resource_metadata="{PUBLIC_HOST}/.well-known/oauth-protected-resource/mcp"',
    })


# --- Well-known endpoints ---

@app.get("/.well-known/oauth-authorization-server")
async def oauth_metadata():
    return {
        "issuer": PUBLIC_HOST,
        "authorization_endpoint": f"{PUBLIC_HOST}/authorize",
        "token_endpoint": f"{PUBLIC_HOST}/token",
        "jwks_uri": f"{PUBLIC_HOST}/.well-known/jwks.json",
        "registration_endpoint": f"{PUBLIC_HOST}/register",
        "response_types_supported": ["code"],
        "grant_types_supported": ["authorization_code", "refresh_token"],
        "code_challenge_methods_supported": ["S256"],
        "token_endpoint_auth_methods_supported": ["none", "client_secret_post", "client_secret_basic"],
        "scopes_supported": ["agents:read", "agents:write"],
    }


@app.get("/.well-known/oauth-protected-resource/mcp")
async def oauth_protected_resource():
    return {
        "resource": f"{PUBLIC_HOST}/mcp",
        "authorization_servers": [PUBLIC_HOST],
        "bearer_methods_supported": ["header"],
        "scopes_supported": ["agents:read", "agents:write"],
    }


@app.get("/.well-known/jwks.json")
async def jwks_endpoint():
    return _jwks_dict()


@app.get("/.well-known/agent-card.json")
async def root_agent_card():
    agents = load_agents()
    if len(agents) == 1:
        name, cfg = next(iter(agents.items()))
        try:
            card = await proxy_get(name, "/.well-known/agent-card.json")
            card["url"] = f"{PUBLIC_HOST}/agents/{name}/a2a"
            return card
        except Exception as e:
            logger.warning(f"Failed to fetch agent card for {name}: {e}")
            return directory_card(agents)
    else:
        return directory_card(agents)


def directory_card(agents):
    skills = []
    for name, cfg in agents.items():
        skills.append({
            "id": name,
            "name": cfg["name"],
            "description": f"Agent: {cfg['name']}",
            "tags": ["agent"],
            "url": f"{PUBLIC_HOST}/agents/{name}/a2a",
        })
    return {
        "name": "Agent Gateway",
        "description": f"Gateway to {len(agents)} agent(s)",
        "url": PUBLIC_HOST,
        "version": "1.0.0",
        "capabilities": {"streaming": True},
        "skills": skills,
    }


# --- Agent proxy ---

def load_agents():
    config_path = "/app/agents.yaml"
    if not os.path.exists(config_path):
        config_path = os.path.join(os.path.dirname(__file__), "agents.yaml")
    with open(config_path) as f:
        config = yaml.safe_load(f)
    agents = {}
    for name, cfg in config.get("agents", {}).items():
        if cfg.get("enabled", True):
            agents[name] = cfg
    return agents


async def proxy_get(agent_name: str, path: str) -> dict:
    agents = load_agents()
    if agent_name not in agents:
        raise HTTPException(status_code=404, detail=f"Agent '{agent_name}' not found")
    base = agents[agent_name]["internal_url"]
    async with httpx.AsyncClient(timeout=30.0) as client:
        resp = await client.get(f"{base}{path}")
        if resp.status_code != 200:
            raise HTTPException(status_code=resp.status_code, detail=resp.text)
        return resp.json()


@app.get("/agents")
async def list_agents():
    agents = load_agents()
    result = []
    for name, cfg in agents.items():
        try:
            card = await proxy_get(name, "/.well-known/agent-card.json")
            card["url"] = f"{PUBLIC_HOST}/agents/{name}/a2a"
        except Exception as e:
            logger.warning(f"Failed to fetch agent card for {name}: {e}")
            card = {"name": cfg["name"], "description": "", "url": f"{PUBLIC_HOST}/agents/{name}/a2a", "version": "0.0.0", "capabilities": {}, "skills": []}
        result.append({"name": name, "config": cfg, "agent_card": card})
    return {"agents": result}


@app.get("/agents/{agent_name}/.well-known/agent-card.json")
async def agent_card_route(agent_name: str):
    card = await proxy_get(agent_name, "/.well-known/agent-card.json")
    card["url"] = f"{PUBLIC_HOST}/agents/{agent_name}/a2a"
    return card


async def proxy_a2a_stream(agent_name: str, body: dict, client: httpx.AsyncClient):
    agents = load_agents()
    base = agents[agent_name]["internal_url"]
    async with client.stream("POST", f"{base}/a2a", json=body, timeout=300.0) as resp:
        async for chunk in resp.aiter_bytes():
            yield chunk


async def proxy_a2a_json(agent_name: str, body: dict) -> dict:
    agents = load_agents()
    base = agents[agent_name]["internal_url"]
    async with httpx.AsyncClient(timeout=300.0) as client:
        resp = await client.post(f"{base}/a2a", json=body)
        if resp.status_code != 200:
            raise HTTPException(status_code=resp.status_code, detail=resp.text)
        return resp.json()


@app.post("/agents/{agent_name}/a2a")
async def agent_a2a(agent_name: str, request: Request):
    agents = load_agents()
    if agent_name not in agents:
        raise HTTPException(status_code=404, detail=f"Agent '{agent_name}' not found")

    try:
        body = await request.json()
    except Exception:
        raise HTTPException(status_code=400, detail="Invalid JSON body")

    method = body.get("method", "")

    if method == "tasks/sendSubscribe":
        client = httpx.AsyncClient(timeout=300.0)
        return StreamingResponse(proxy_a2a_stream(agent_name, body, client), media_type="text/event-stream")

    result = await proxy_a2a_json(agent_name, body)
    return JSONResponse(content=result)


# --- MCP Protocol Endpoint ---

def _first_agent() -> tuple[str, dict] | None:
    agents = load_agents()
    for name, cfg in agents.items():
        return name, cfg
    return None


async def _call_a2a_research(query: str) -> str:
    agent_info = _first_agent()
    if not agent_info:
        raise HTTPException(status_code=503, detail="No agents available")
    name, cfg = agent_info
    base = cfg["internal_url"]

    body = {
        "jsonrpc": "2.0", "id": 1, "method": "tasks/send",
        "params": {"query": query, "id": str(uuid.uuid4()), "sessionId": str(uuid.uuid4()), "historyLength": 0},
    }
    async with httpx.AsyncClient(timeout=300.0) as client:
        resp = await client.post(f"{base}/a2a", json=body)
        if resp.status_code != 200:
            return f"Error submitting research task: HTTP {resp.status_code}"
        send_data = resp.json()
        task_id = send_data.get("result", {}).get("id", "")
        if not task_id:
            return f"Error: no task id in response: {send_data}"

        for _ in range(60):
            await asyncio.sleep(5)
            resp = await client.post(f"{base}/a2a", json={
                "jsonrpc": "2.0", "id": 1, "method": "tasks/get",
                "params": {"id": task_id},
            })
            if resp.status_code != 200:
                continue
            data = resp.json().get("result", {})
            status = data.get("status")
            if status == "completed":
                final = data.get("final_report") or data.get("result") or ""
                if isinstance(final, str):
                    return final
                return json.dumps(final)
            elif status == "failed":
                return f"Research failed: {data.get('error', 'unknown error')}"

        return "Research timed out"


def _sse_response(data: dict, status: int = 200) -> Response:
    session_id = uuid.uuid4().hex
    body = f"event: message\ndata: {json.dumps(data)}\n\n"
    return Response(
        content=body,
        status_code=status,
        media_type="text/event-stream",
        headers={"mcp-session-id": session_id},
    )


@app.post("/mcp")
@app.post("/")
async def mcp_handler(request: Request):
    try:
        body = await request.json()
    except Exception:
        return _sse_response({"jsonrpc": "2.0", "error": {"code": -32700, "message": "Parse error"}, "id": None}, 400)

    method = body.get("method", "")
    req_id = body.get("id")

    if method == "initialize":
        return _sse_response({
            "jsonrpc": "2.0", "id": req_id, "result": {
                "protocolVersion": "2024-11-05",
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "Agent Gateway", "version": "1.0.0"},
            }
        })

    elif method == "notifications/initialized":
        return _sse_response({"jsonrpc": "2.0", "id": req_id, "result": {}}, 202)

    elif method == "tools/list":
        agent = _first_agent()
        tools = []
        if agent:
            name, cfg = agent
            try:
                card = await proxy_get(name, "/.well-known/agent-card.json")
                desc = card.get("description", f"Research agent: {cfg['name']}")
                skills = card.get("skills", [])
                for skill in skills:
                    tools.append({
                        "name": skill.get("id", name),
                        "description": skill.get("description", desc),
                        "inputSchema": {
                            "type": "object",
                            "properties": {
                                "query": {"type": "string", "description": "Research query"},
                            },
                            "required": ["query"],
                        },
                    })
            except Exception:
                tools.append({
                    "name": name,
                    "description": cfg.get("name", "Research agent"),
                    "inputSchema": {
                        "type": "object",
                        "properties": {
                            "query": {"type": "string", "description": "Research query"},
                        },
                        "required": ["query"],
                    },
                })
        return _sse_response({"jsonrpc": "2.0", "id": req_id, "result": {"tools": tools}})

    elif method == "tools/call":
        if not getattr(request.state, "authenticated", False):
            return _sse_response({"jsonrpc": "2.0", "id": req_id, "error": {
                "code": -32001, "message": "Not authenticated",
            }}, 401)

        params = body.get("params", {})
        tool_name = params.get("name", "")
        arguments = params.get("arguments", {})

        agent = _first_agent()
        if not agent:
            return _sse_response({"jsonrpc": "2.0", "id": req_id, "error": {
                "code": -32000, "message": "No agents available",
            }})

        query = arguments.get("query", "")
        if not query:
            return _sse_response({"jsonrpc": "2.0", "id": req_id, "error": {
                "code": -32602, "message": "Missing query",
            }})

        try:
            result = await _call_a2a_research(query)
            return _sse_response({"jsonrpc": "2.0", "id": req_id, "result": {
                "content": [{"type": "text", "text": result}],
                "isError": False,
            }})
        except Exception as e:
            logger.warning(f"MCP tools/call failed: {e}")
            return _sse_response({"jsonrpc": "2.0", "id": req_id, "error": {
                "code": -32000, "message": str(e),
            }})

    return _sse_response({"jsonrpc": "2.0", "id": req_id, "error": {
        "code": -32601, "message": f"Method not found: {method}",
    }})


@app.get("/")
async def root_get():
    agent = _first_agent()
    if agent:
        try:
            card = await proxy_get(agent[0], "/.well-known/agent-card.json")
            return card
        except Exception:
            pass
    return {"name": "Agent Gateway", "version": "1.0.0", "description": "A2A Agent Gateway with MCP support"}


@app.get("/health")
async def health():
    return {"status": "ok"}