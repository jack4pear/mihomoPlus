#!/usr/bin/env python3
"""Offline binary smoke test with two controllable SOCKS5 nodes and an HTTP origin."""
import contextlib
import http.client
import http.server
import json
import pathlib
import select
import socket
import socketserver
import struct
import subprocess
import tempfile
import threading
import time
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parents[1]


def exact(sock, size):
    data = b""
    while len(data) < size:
        part = sock.recv(size - len(data))
        if not part:
            raise EOFError()
        data += part
    return data


class SocksServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True

    def __init__(self):
        self.failed = threading.Event()
        super().__init__(("127.0.0.1", 0), SocksHandler)


class SocksHandler(socketserver.BaseRequestHandler):
    def handle(self):
        try:
            self.request.settimeout(3)
            version, methods = exact(self.request, 2)
            exact(self.request, methods)
            if version != 5 or self.server.failed.is_set():
                return
            self.request.sendall(b"\x05\x00")
            version, command, _, kind = exact(self.request, 4)
            if command != 1:
                return
            if kind == 1:
                host = socket.inet_ntoa(exact(self.request, 4))
            elif kind == 3:
                host = exact(self.request, exact(self.request, 1)[0]).decode()
            elif kind == 4:
                host = socket.inet_ntop(socket.AF_INET6, exact(self.request, 16))
            else:
                return
            port = struct.unpack("!H", exact(self.request, 2))[0]
            with socket.create_connection((host, port), timeout=3) as upstream:
                self.request.sendall(b"\x05\x00\x00\x01\x7f\x00\x00\x01\x00\x00")
                while True:
                    readable, _, _ = select.select([self.request, upstream], [], [], 3)
                    if not readable:
                        return
                    for source in readable:
                        chunk = source.recv(65536)
                        if not chunk:
                            return
                        (upstream if source is self.request else self.request).sendall(chunk)
        except (OSError, EOFError):
            return


class OriginHandler(http.server.BaseHTTPRequestHandler):
    def do_HEAD(self):
        self.send_response(204)
        self.end_headers()

    def do_GET(self):
        body = b"smart-fallback-smoke-ok"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass


def free_port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def wait_for(predicate, label, timeout=35):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if predicate():
                print(label, flush=True)
                return
        except (OSError, ValueError, http.client.HTTPException):
            pass
        time.sleep(0.1)
    raise AssertionError("timed out: " + label)


def main():
    binary = ROOT / "dist/mihomo-smart-linux-amd64"
    logs = ROOT / "dist/validation"
    logs.mkdir(parents=True, exist_ok=True)
    kernel_log = logs / "smoke-kernel.log"
    servers = [SocksServer(), SocksServer(),
               http.server.ThreadingHTTPServer(("127.0.0.1", 0), OriginHandler)]
    process = None
    try:
        for server in servers:
            threading.Thread(target=server.serve_forever, daemon=True).start()
        a, b, origin = servers
        api_port, mixed_port = free_port(), free_port()
        with tempfile.TemporaryDirectory(prefix="mihomoplus-smoke-") as temp:
            config = pathlib.Path(temp) / "config.yaml"
            config.write_text(f"""
mixed-port: {mixed_port}
external-controller: 127.0.0.1:{api_port}
allow-lan: false
mode: rule
log-level: info
proxies:
  - {{name: A, type: socks5, server: 127.0.0.1, port: {a.server_address[1]}}}
  - {{name: B, type: socks5, server: 127.0.0.1, port: {b.server_address[1]}}}
proxy-groups:
  - name: Smart
    type: fallback
    proxies: [A, B]
    url: http://127.0.0.1:{origin.server_address[1]}/health
    expected-status: 204
    interval: 2
    timeout: 500
    lazy: false
    smart-fallback:
      enabled: true
      window: 10
      failure-threshold: 2
      cooldown: [4, 6, 8]
      recover-success: 3
      reset-after: 10
rules:
  - MATCH,Smart
""")
            subprocess.run([str(binary), "-t", "-d", temp, "-f", str(config)], check=True)
            with kernel_log.open("w") as log:
                process = subprocess.Popen([str(binary), "-d", temp, "-f", str(config)],
                                           stdout=log, stderr=subprocess.STDOUT)
                opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

                def now():
                    if process.poll() is not None:
                        raise RuntimeError("kernel exited unexpectedly")
                    with opener.open(f"http://127.0.0.1:{api_port}/proxies/Smart", timeout=2) as response:
                        return json.load(response)["now"]

                def log_has(text):
                    return text in kernel_log.read_text()

                def business():
                    conn = http.client.HTTPConnection("127.0.0.1", mixed_port, timeout=2)
                    try:
                        conn.request("GET", f"http://127.0.0.1:{origin.server_address[1]}/business")
                        response = conn.getresponse()
                        return response.status == 200 and response.read() == b"smart-fallback-smoke-ok"
                    finally:
                        conn.close()

                wait_for(lambda: now() == "A", "initial priority selects A")
                assert business(), "initial proxy business failed"
                a.failed.set()
                wait_for(lambda: now() == "B" and log_has("cooldown A"), "A trips; B carries traffic")
                assert business(), "B proxy business failed"
                a.failed.clear()
                wait_for(lambda: log_has("recovered A") and now() == "A",
                         "three successful probes restore A priority")
                assert business(), "recovered A business failed"

                a.failed.set()
                b.failed.set()
                wait_for(lambda: now() == "SMART-FALLBACK-UNAVAILABLE" and log_has("cooldown B"),
                         "all failed nodes produce unavailable selection")
                with contextlib.suppress(OSError, http.client.HTTPException):
                    assert not business(), "all-tripped nodes unexpectedly carried traffic"

                b.failed.clear()
                wait_for(lambda: now() == "B" and log_has("recovered B"),
                         "emergency verification restores B after three successes")
                assert business(), "emergency recovered B business failed"
                print("PASS: offline binary fault-injection smoke test", flush=True)
    finally:
        if process is not None:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        for server in servers:
            server.shutdown()
            server.server_close()


if __name__ == "__main__":
    main()
