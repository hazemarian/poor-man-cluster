import json
import threading
import unittest
from urllib.request import urlopen

from obsai.config import Settings
from obsai.evidence import build_brief
from obsai.http import create_server
from obsai.parseable import QueryResult


_ESCAPED_SECRET_JSON = json.dumps({"password": 'secret"remainingsecret'})


class FakeClient:
    stream = "pmcluster-logs"

    def query_logs(self, _window_hours: int) -> QueryResult:
        return QueryResult(
            rows=[
                {
                    "service.name": "api",
                    "severity_text": "ERROR",
                    "body": f"request failed after 2ms token=sensitivevalue {_ESCAPED_SECRET_JSON}",
                    "p_timestamp": "2026-10-17T10:00:00Z",
                }
            ]
        )


class EmptyClient:
    stream = "pmcluster-logs"

    def query_logs(self, _window_hours: int) -> QueryResult:
        return QueryResult(rows=[])


class HTTPTests(unittest.TestCase):
    def setUp(self) -> None:
        self.server = create_server(Settings(bind_host="127.0.0.1", bind_port=0), FakeClient())
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base = f"http://127.0.0.1:{self.server.server_port}"

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def test_health_is_local(self) -> None:
        with urlopen(f"{self.base}/api/health") as response:
            payload = json.loads(response.read())
        self.assertEqual(payload["status"], "ok")

    def test_brief_returns_evidence(self) -> None:
        with urlopen(f"{self.base}/api/brief?window=2h") as response:
            payload = json.loads(response.read())
        self.assertEqual(payload["status"], "ok")
        self.assertEqual(payload["evidence"]["templates"][0]["service"], "api")
        self.assertIn("service\tseverity", payload["evidence"]["tsv"])
        self.assertNotIn("sensitivevalue", json.dumps(payload))

    def test_empty_source_is_unknown(self) -> None:
        payload = build_brief(EmptyClient(), Settings())
        self.assertEqual(payload["status"], "unknown")
        self.assertTrue(payload["source"]["unknown"])
        self.assertTrue(payload["evidence"]["unknown"])
