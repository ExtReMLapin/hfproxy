#!/usr/bin/env python3
"""Downloads a large file through a real hfproxy process and reports what it cost.

The Go load test keeps its object store in the test process, so it cannot say
how much memory the proxy needs for a multi-gigabyte file. This script starts the
real binary as a separate process, serves a file of the requested size from a
stand-in Hub (generated on the fly, with the SHA-256 the Hub would advertise),
and samples the proxy's resident memory and its staging directory while it:

  1. fills the cache (a cold download, verified against the digest),
  2. serves the file again from the cache,
  3. serves a range from the middle of it.

It fails on a wrong byte, a leftover staging file, or memory above --max-rss-mib.

Needs PostgreSQL and an S3 service able to hold the file (moto keeps objects in
RAM: use Garage or MinIO for more than a couple of GiB). Configure them with the
usual variables, for example:

  export HFPROXY_DATABASE_URL=postgres://u:p@127.0.0.1:5432/scratch?sslmode=disable
  export HFPROXY_S3_ENDPOINT=http://127.0.0.1:3900 HFPROXY_S3_REGION=garage HFPROXY_S3_BUCKET=hfproxy \
         HFPROXY_S3_ACCESS_KEY=... HFPROXY_S3_SECRET_KEY=... HFPROXY_S3_FORCE_PATH_STYLE=true \
         HFPROXY_S3_SERVER_SIDE_ENCRYPTION=false
  scripts/large-file-test.py --binary ./hfproxy --size-gib 8

Use an empty database: the script creates the first administrator.
"""
import argparse
import hashlib
import http.cookiejar
import http.server
import json
import os
import random
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request

BLOCK_SIZE = 1 << 20
BLOCK = random.Random(20260429).randbytes(BLOCK_SIZE)
COMMIT = "0123456789abcdef0123456789abcdef01234567"
SETUP_CODE = "large-file-test-setup-code"


def pattern_slice(start, length):
    """Bytes [start, start+length) of the generated file (BLOCK repeated)."""
    out = bytearray()
    while length > 0:
        offset = start % BLOCK_SIZE
        take = min(length, BLOCK_SIZE - offset)
        out += BLOCK[offset : offset + take]
        start += take
        length -= take
    return bytes(out)


def file_sha256(size):
    h = hashlib.sha256()
    full, rest = divmod(size, BLOCK_SIZE)
    for _ in range(full):
        h.update(BLOCK)
    h.update(BLOCK[:rest])
    return h.hexdigest()


class Hub(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    size = 0
    digest = ""
    port = 0

    def log_message(self, *args):
        pass

    def do_HEAD(self):
        self.serve(False)

    def do_GET(self):
        self.serve(True)

    def serve(self, body):
        path = self.path.split("?")[0]
        if path.endswith("/big.bin") and "/resolve/" in path:
            self.send_response(302)
            self.send_header("X-Repo-Commit", COMMIT)
            self.send_header("X-Linked-Etag", '"%s"' % self.digest)
            self.send_header("X-Linked-Size", str(self.size))
            self.send_header("Location", "http://127.0.0.1:%d/cdn/big.bin" % self.port)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if path == "/cdn/big.bin":
            start, end, status = 0, self.size - 1, 200
            header = self.headers.get("Range")
            if header and header.startswith("bytes="):
                first, _, last = header[6:].partition("-")
                start = int(first)
                end = int(last) if last else self.size - 1
                status = 206
            self.send_response(status)
            self.send_header("ETag", '"cdn"')
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(end - start + 1))
            if status == 206:
                self.send_header("Content-Range", "bytes %d-%d/%d" % (start, end, self.size))
            self.end_headers()
            if body:
                position = start
                try:
                    while position <= end:
                        chunk = pattern_slice(position, min(BLOCK_SIZE, end - position + 1))
                        self.wfile.write(chunk)
                        position += len(chunk)
                except (BrokenPipeError, ConnectionResetError):
                    pass
            return
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()


class Sampler(threading.Thread):
    """Samples the proxy's resident memory and the size of its staging directory."""

    def __init__(self, pid, staging):
        super().__init__(daemon=True)
        self.pid, self.staging = pid, staging
        self.stop_flag = threading.Event()
        self.reset()

    def reset(self):
        self.peak_rss = 0
        self.peak_staging = 0

    def rss(self):
        with open("/proc/%d/status" % self.pid) as f:
            for line in f:
                if line.startswith("VmRSS:"):
                    return int(line.split()[1]) * 1024
        return 0

    def hwm(self):
        with open("/proc/%d/status" % self.pid) as f:
            for line in f:
                if line.startswith("VmHWM:"):
                    return int(line.split()[1]) * 1024
        return 0

    def staging_bytes(self):
        total = 0
        for root, _, files in os.walk(self.staging):
            for name in files:
                try:
                    total += os.path.getsize(os.path.join(root, name))
                except OSError:
                    pass
        return total

    def run(self):
        while not self.stop_flag.is_set():
            try:
                self.peak_rss = max(self.peak_rss, self.rss())
                self.peak_staging = max(self.peak_staging, self.staging_bytes())
            except OSError:
                return
            time.sleep(0.1)


def mib(n):
    return "%.0f MiB" % (n / (1 << 20))


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--binary", default="./hfproxy")
    ap.add_argument("--size-gib", type=float, default=2.0)
    ap.add_argument("--max-rss-mib", type=int, default=600, help="fail when the proxy's resident memory exceeds this")
    ap.add_argument("--port", type=int, default=18480)
    ap.add_argument("--hub-port", type=int, default=18481)
    ap.add_argument("--staging", default="", help="staging directory (default: a temporary one)")
    args = ap.parse_args()

    if not os.access(args.binary, os.X_OK):
        sys.exit("binary %s not found; build it with: go build -o hfproxy ./cmd/hfproxy" % args.binary)
    for name in ("HFPROXY_DATABASE_URL", "HFPROXY_S3_BUCKET"):
        if not os.environ.get(name):
            sys.exit("%s is not set (see the top of this script)" % name)
    size = int(args.size_gib * (1 << 30))
    staging = args.staging or tempfile.mkdtemp(prefix="hfproxy-staging-")
    os.makedirs(staging, exist_ok=True)
    free = shutil.disk_usage(staging).free
    if free < size * 1.3:
        sys.exit("the staging directory has %s free; a %s fill needs more" % (mib(free), mib(size)))

    print("hashing %s of generated data..." % mib(size), flush=True)
    Hub.size, Hub.digest, Hub.port = size, file_sha256(size), args.hub_port
    hub = http.server.ThreadingHTTPServer(("127.0.0.1", args.hub_port), Hub)
    threading.Thread(target=hub.serve_forever, daemon=True).start()

    env = dict(os.environ)
    env.update({
        "HFPROXY_LISTEN_ADDR": "127.0.0.1:%d" % args.port,
        "HFPROXY_PUBLIC_URL": "http://127.0.0.1:%d" % args.port,
        "HFPROXY_HF_BASE_URL": "http://127.0.0.1:%d" % args.hub_port,
        "HFPROXY_HF_ALLOWED_REDIRECT_HOSTS": "127.0.0.1",
        "HFPROXY_AUTO_MIGRATE": "true",
        "HFPROXY_ANONYMOUS_PUBLIC_READ": "true",
        "HFPROXY_SETUP_TOKEN": SETUP_CODE,
        "HFPROXY_STAGING_DIR": staging,
        "HFPROXY_LOG_LEVEL": "warn",
    })
    env.setdefault("HFPROXY_SESSION_SECRET", "large-file-test-session-secret-0123456789")
    env.setdefault("HFPROXY_S3_REGION", "us-east-1")
    log = open(os.path.join(tempfile.gettempdir(), "hfproxy-large-file.log"), "w")
    proxy = subprocess.Popen([args.binary, "serve"], env=env, stdout=log, stderr=log)
    base = "http://127.0.0.1:%d" % args.port
    failures = []
    try:
        for _ in range(60):
            try:
                urllib.request.urlopen(base + "/readyz", timeout=2).read()
                break
            except Exception:
                if proxy.poll() is not None:
                    sys.exit("the proxy exited at start; see %s" % log.name)
                time.sleep(1)
        else:
            sys.exit("the proxy never became ready; see %s" % log.name)

        jar = http.cookiejar.CookieJar()
        opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))

        def post(path, payload, csrf=None):
            req = urllib.request.Request(base + path, data=json.dumps(payload).encode(), method="POST",
                                         headers={"Content-Type": "application/json"})
            if csrf:
                req.add_header("X-CSRF-Token", csrf)
            return opener.open(req, timeout=30)

        post("/api/v1/setup", {"setup_code": SETUP_CODE, "username": "root", "password": "a-long-enough-password"})
        csrf = next(c.value for c in jar if c.name == "hfproxy_csrf")
        post("/api/v1/repos", {"repo_type": "models", "repo_id": "org/model", "access_policy": "public"}, csrf)

        sampler = Sampler(proxy.pid, staging)
        sampler.start()
        time.sleep(0.5)
        baseline = sampler.rss()
        results = []

        def download(label, headers=None, expect_len=None, expect_hash=None):
            sampler.reset()
            req = urllib.request.Request(base + "/org/model/resolve/main/big.bin", headers=headers or {})
            started = time.time()
            h, n = hashlib.sha256(), 0
            with urllib.request.urlopen(req, timeout=3600) as resp:
                status, cache = resp.status, resp.headers.get("X-Cache", "")
                while True:
                    chunk = resp.read(1 << 20)
                    if not chunk:
                        break
                    h.update(chunk)
                    n += len(chunk)
            took = time.time() - started
            time.sleep(0.3)
            ok = (expect_len is None or n == expect_len) and (expect_hash is None or h.hexdigest() == expect_hash)
            if not ok:
                failures.append("%s: %d bytes, digest %s" % (label, n, h.hexdigest()))
            results.append((label, status, cache, n, took, sampler.peak_rss, sampler.peak_staging, ok))

        download("cold fill", expect_len=size, expect_hash=Hub.digest)
        download("cached read", expect_len=size, expect_hash=Hub.digest)
        window = min(128 << 20, size // 2)
        first = size // 3
        expected = hashlib.sha256(pattern_slice(first, window)).hexdigest()
        download("range read", {"Range": "bytes=%d-%d" % (first, first + window - 1)}, window, expected)

        time.sleep(1)
        leftovers = [f for _, _, files in os.walk(staging) for f in files]
        sampler.stop_flag.set()
        hwm = sampler.hwm()
        print()
        print("file %s, proxy idle at %s" % (mib(size), mib(baseline)))
        print("%-12s %6s %-6s %12s %9s %10s %12s" % ("phase", "status", "cache", "bytes", "MiB/s", "peak RSS", "peak staging"))
        for label, status, cache, n, took, rss, stg, ok in results:
            print("%-12s %6d %-6s %12d %9.0f %10s %12s %s" % (label, status, cache or "-", n, n / (1 << 20) / max(took, 1e-9), mib(rss), mib(stg), "" if ok else "WRONG"))
        print("proxy peak memory over the whole run (VmHWM): %s" % mib(hwm))
        if leftovers:
            failures.append("%d staging files left behind" % len(leftovers))
        if hwm > args.max_rss_mib << 20:
            failures.append("peak memory %s is above --max-rss-mib %d" % (mib(hwm), args.max_rss_mib))
    finally:
        proxy.send_signal(signal.SIGTERM)
        try:
            proxy.wait(timeout=30)
        except subprocess.TimeoutExpired:
            proxy.kill()
        hub.shutdown()
        if not args.staging:
            shutil.rmtree(staging, ignore_errors=True)
    if failures:
        print("\nFAILED:\n  " + "\n  ".join(failures))
        sys.exit(1)
    print("\nOK")


if __name__ == "__main__":
    main()
