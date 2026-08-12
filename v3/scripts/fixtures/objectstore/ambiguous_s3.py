#!/usr/bin/env python3
"""Minimal local S3-shaped fault fixture for Phase-0 transport testing.

This is deliberately not a production object store and never leaves the
loopback interface. It accepts the subset of S3 requests emitted by AWS CLI
for the Phase-0 probe. The first conditional PUT for --fail-key persists the
bytes and closes the socket before sending a response, creating an ambiguous
client-visible outcome. A later conditional PUT receives 412, while GET
returns the persisted bytes.
"""

from __future__ import annotations

import argparse
import hashlib
import http.server
import os
import signal
import ssl
import threading
import urllib.parse
import xml.etree.ElementTree as ET


class Store:
    def __init__(self, bucket: str, fail_key: str) -> None:
        self.bucket = bucket
        self.fail_key = fail_key
        self.objects: dict[str, bytes] = {}
        self.failed_once = False
        self.lock = threading.Lock()


def error_xml(code: str, message: str) -> bytes:
    root = ET.Element("Error")
    ET.SubElement(root, "Code").text = code
    ET.SubElement(root, "Message").text = message
    ET.SubElement(root, "RequestId").text = "phase0-fixture"
    return ET.tostring(root, encoding="utf-8", xml_declaration=True)


class Handler(http.server.BaseHTTPRequestHandler):
    server_version = "agw-phase0-fixture/1"

    @property
    def store(self) -> Store:
        return self.server.store  # type: ignore[attr-defined]

    def log_message(self, _format: str, *_args: object) -> None:
        # Request paths can contain user-controlled object keys. Keep the
        # fixture quiet so evidence cannot accidentally contain payload data.
        return

    def object_key(self) -> tuple[str | None, str | None]:
        parsed = urllib.parse.urlsplit(self.path)
        segments = [urllib.parse.unquote(part) for part in parsed.path.split("/") if part]
        host = self.headers.get("Host", "").split(":", 1)[0]
        bucket = segments[0] if segments else None
        key = "/".join(segments[1:])
        if host.startswith(self.store.bucket + ".") and segments:
            bucket = self.store.bucket
            key = "/".join(segments)
        return bucket, key or None

    def send_error_xml(self, status: int, code: str, message: str) -> None:
        body = error_xml(code, message)
        self.send_response(status)
        self.send_header("Content-Type", "application/xml")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_HEAD(self) -> None:  # noqa: N802
        bucket, key = self.object_key()
        if bucket != self.store.bucket:
            self.send_error_xml(404, "NoSuchBucket", "bucket not found")
            return
        if key is None:
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        with self.store.lock:
            body = self.store.objects.get(key)
        if body is None:
            self.send_error_xml(404, "NoSuchKey", "object not found")
            return
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("ETag", '"%s"' % hashlib.md5(body).hexdigest())
        self.send_header("x-amz-meta-agw-phase0-run", "fixture")
        self.end_headers()

    def do_GET(self) -> None:  # noqa: N802
        bucket, key = self.object_key()
        if bucket != self.store.bucket or key is None:
            self.send_error_xml(404, "NoSuchKey", "object not found")
            return
        with self.store.lock:
            body = self.store.objects.get(key)
        if body is None:
            self.send_error_xml(404, "NoSuchKey", "object not found")
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/octet-stream")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("ETag", '"%s"' % hashlib.md5(body).hexdigest())
        self.end_headers()
        self.wfile.write(body)

    def do_PUT(self) -> None:  # noqa: N802
        bucket, key = self.object_key()
        if bucket != self.store.bucket or key is None:
            self.send_error_xml(404, "NoSuchKey", "object not found")
            return
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        conditional = self.headers.get("If-None-Match") == "*"
        with self.store.lock:
            exists = key in self.store.objects
            if conditional and exists:
                self.send_error_xml(412, "PreconditionFailed", "object already exists")
                return
            self.store.objects[key] = body
            should_drop = key == self.store.fail_key and not self.store.failed_once
            if should_drop:
                self.store.failed_once = True
        if should_drop:
            self.close_connection = True
            try:
                self.connection.shutdown(2)
            except OSError:
                pass
            return
        self.send_response(200)
        self.send_header("ETag", '"%s"' % hashlib.md5(body).hexdigest())
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_DELETE(self) -> None:  # noqa: N802
        bucket, key = self.object_key()
        if bucket != self.store.bucket or key is None:
            self.send_error_xml(404, "NoSuchKey", "object not found")
            return
        with self.store.lock:
            self.store.objects.pop(key, None)
        self.send_response(204)
        self.send_header("Content-Length", "0")
        self.end_headers()


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address: tuple[str, int], store: Store) -> None:
        super().__init__(address, Handler)
        self.store = store


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", required=True)
    parser.add_argument("--port", required=True, type=int)
    parser.add_argument("--bucket", required=True)
    parser.add_argument("--fail-key", required=True)
    parser.add_argument("--ready-file", required=True)
    parser.add_argument("--tls-cert")
    parser.add_argument("--tls-key")
    args = parser.parse_args()
    if args.host not in {"127.0.0.1", "localhost"}:
        raise SystemExit("fixture host must be loopback")
    if not args.bucket or "/" in args.bucket:
        raise SystemExit("invalid fixture bucket")
    if bool(args.tls_cert) != bool(args.tls_key):
        raise SystemExit("--tls-cert and --tls-key must be supplied together")
    server = Server((args.host, args.port), Store(args.bucket, args.fail_key))
    scheme = "http"
    if args.tls_cert:
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(args.tls_cert, args.tls_key)
        server.socket = context.wrap_socket(server.socket, server_side=True)
        scheme = "https"
    ready_parent = os.path.dirname(os.path.abspath(args.ready_file))
    os.makedirs(ready_parent, mode=0o700, exist_ok=True)
    with open(args.ready_file, "w", encoding="utf-8") as ready:
        ready.write("endpoint=%s://%s:%d\n" % (scheme, args.host, server.server_address[1]))
        ready.flush()

    stop = threading.Event()

    def request_stop(_signum: int, _frame: object) -> None:
        if not stop.is_set():
            stop.set()
            threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, request_stop)
    signal.signal(signal.SIGINT, request_stop)
    try:
        server.serve_forever(poll_interval=0.1)
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
