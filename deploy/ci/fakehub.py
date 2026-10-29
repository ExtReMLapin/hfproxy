"""A tiny stand-in for the Hugging Face Hub, used by the cluster smoke test.

It serves one small file (config.json) and one 1 MiB "LFS" file (model.bin)
behind a redirect, supports Range requests, and counts the requests that
reach it (GET /_hits) so the test can tell a cache hit from a fetch.
"""
import hashlib
import http.server

COMMIT = "0123456789abcdef0123456789abcdef01234567"
BIG = b"weights-" * 131072  # 1 MiB
SHA = hashlib.sha256(BIG).hexdigest()
SMALL = b'{"model_type":"bert"}'
hits = {"small": 0, "big": 0}


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def do_HEAD(self):
        self.serve(False)

    def do_GET(self):
        self.serve(True)

    def reply(self, status, headers, body=b"", send_body=True):
        self.send_response(status)
        for key, value in headers.items():
            self.send_header(key, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if send_body and body:
            self.wfile.write(body)

    def serve(self, send_body):
        path = self.path.split("?")[0]
        if path == "/_hits":
            self.reply(200, {"Content-Type": "application/json"}, ('{"small":%d,"big":%d}' % (hits["small"], hits["big"])).encode())
            return
        if path.endswith("/resolve/main/config.json") or path.endswith("/resolve/" + COMMIT + "/config.json"):
            if send_body:
                hits["small"] += 1
            self.reply(200, {"X-Repo-Commit": COMMIT, "ETag": '"cfg-etag"', "Content-Type": "application/json"}, SMALL, send_body)
            return
        if path.endswith("/resolve/main/model.bin") or path.endswith("/resolve/" + COMMIT + "/model.bin"):
            location = "http://" + self.headers.get("Host", "fakehub:8080") + "/cdn/model.bin"
            self.reply(302, {"X-Repo-Commit": COMMIT, "X-Linked-Etag": '"' + SHA + '"', "X-Linked-Size": str(len(BIG)), "Location": location})
            return
        if path == "/cdn/model.bin":
            data, status, headers = BIG, 200, {"ETag": '"cdn"', "Content-Type": "application/octet-stream"}
            rng = self.headers.get("Range")
            if rng and rng.startswith("bytes="):
                first, last = rng[6:].split("-")
                first = int(first)
                last = int(last) if last else len(BIG) - 1
                data, status = BIG[first : last + 1], 206
                headers["Content-Range"] = "bytes %d-%d/%d" % (first, last, len(BIG))
            if send_body:
                hits["big"] += 1
            self.reply(status, headers, data, send_body)
            return
        self.reply(404, {"X-Error-Code": "EntryNotFound"})


http.server.ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
