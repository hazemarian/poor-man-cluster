import json
import unittest
from urllib.error import URLError

from obsai.parseable import ParseableClient, ParseableError


class FakeResponse:
    status = 200

    def __init__(self, payload: object) -> None:
        self.body = json.dumps(payload).encode()

    def __enter__(self) -> "FakeResponse":
        return self

    def __exit__(self, *_args: object) -> None:
        return None

    def read(self) -> bytes:
        return self.body


class ParseableTests(unittest.TestCase):
    def test_query_is_select_only_and_bounded(self) -> None:
        requests = []

        def opener(request, **_kwargs):
            requests.append(request)
            return FakeResponse({"data": [{"body": "ok"}]})

        client = ParseableClient("http://parseable", max_rows=10, opener=opener)
        result = client.query("SELECT * FROM `pmcluster-logs`")
        self.assertEqual(result.rows, [{"body": "ok"}])
        self.assertIn("LIMIT 10", json.loads(requests[0].data)["query"])
        with self.assertRaises(ParseableError):
            client.query("DELETE FROM `pmcluster-logs`")
        with self.assertRaises(ParseableError):
            client.query("SELECT 1; DROP TABLE logs")

    def test_existing_limit_is_clamped(self) -> None:
        requests = []

        def opener(request, **_kwargs):
            requests.append(request)
            return FakeResponse({"result": []})

        client = ParseableClient("http://parseable", max_rows=10, opener=opener)
        client.query("SELECT * FROM logs LIMIT 999")
        self.assertIn("LIMIT 10", json.loads(requests[0].data)["query"])

    def test_network_failure_is_explicit(self) -> None:
        def opener(_request, **_kwargs):
            raise URLError("offline")

        client = ParseableClient("http://parseable", opener=opener)
        with self.assertRaises(ParseableError) as caught:
            client.query_logs(2)
        self.assertIn("Parseable query failed", str(caught.exception))

    def test_logs_query_sends_explicit_window_bounds(self) -> None:
        requests = []

        def opener(request, **_kwargs):
            requests.append(request)
            return FakeResponse({"data": []})

        client = ParseableClient("http://parseable", max_rows=10, opener=opener)
        client.query_logs(2)
        payload = json.loads(requests[0].data)
        self.assertRegex(payload["startTime"], r"^20\d\d-.*Z$")
        self.assertRegex(payload["endTime"], r"^20\d\d-.*Z$")
        self.assertIn("INTERVAL '2 hours'", payload["query"])

    def test_malformed_success_response_is_an_error(self) -> None:
        def opener(_request, **_kwargs):
            return FakeResponse({"error": "query rejected"})

        client = ParseableClient("http://parseable", opener=opener)
        with self.assertRaises(ParseableError):
            client.query_logs(2)

    def test_nested_malformed_success_response_is_an_error(self) -> None:
        def opener(_request, **_kwargs):
            return FakeResponse({"data": {"error": "query rejected"}})

        client = ParseableClient("http://parseable", opener=opener)
        with self.assertRaises(ParseableError):
            client.query_logs(2)
