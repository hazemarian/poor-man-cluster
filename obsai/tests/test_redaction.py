import json
import unittest

from obsai.redaction import redact_text


class RedactionTests(unittest.TestCase):
    def test_masks_credentials_and_identifiers(self) -> None:
        text = (
            "password=super-secret token:tok-value "
            "Authorization: Bearer abcdefghijkl "
            "user@example.com 192.0.2.10:443"
        )
        result = redact_text(text, exact_values=["super-secret"])
        self.assertNotIn("super-secret", result)
        self.assertNotIn("tok-value", result)
        self.assertNotIn("abcdefghijkl", result)
        self.assertNotIn("user@example.com", result)
        self.assertNotIn("192.0.2.10", result)
        self.assertIn("password=<REDACTED>", result)
        self.assertIn("Bearer <REDACTED>", result)

    def test_keeps_error_context(self) -> None:
        result = redact_text("redis dial failed: i/o timeout")
        self.assertEqual(result, "redis dial failed: i/o timeout")

    def test_masks_json_and_quoted_values(self) -> None:
        result = redact_text('{"password":"hunter2", "token": "two words secret"}')
        self.assertNotIn("hunter2", result)
        self.assertNotIn("two words secret", result)
        self.assertIn('"password":"<REDACTED>"', result)

    def test_masks_escaped_quotes_in_json_secret(self) -> None:
        payload = json.dumps({"password": 'secret"remainingsecret'})
        result = redact_text(payload)
        self.assertNotIn("secret", result)
        self.assertNotIn("remainingsecret", result)
        self.assertIn('"password": "<REDACTED>"', result)
