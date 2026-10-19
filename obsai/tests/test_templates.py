import unittest

from obsai.templates import TemplateAggregator, normalize_message


class TemplateTests(unittest.TestCase):
    def test_normalizes_volatile_values(self) -> None:
        first = normalize_message("request 123 failed after 4.2ms")
        second = normalize_message("request 987 failed after 7.1ms")
        self.assertEqual(first, second)
        self.assertIn("<N>", first)
        self.assertIn("<DUR>", first)

    def test_aggregates_and_redacts(self) -> None:
        rows = [
            {
                "service.name": "checkout",
                "severity_text": "ERROR",
                "body": "password=secret-value redis dial failed after 2ms",
                "p_timestamp": "2026-10-17T10:00:00Z",
            },
            {
                "service.name": "checkout",
                "severity_text": "ERROR",
                "body": "password=secret-value redis dial failed after 3ms",
                "p_timestamp": "2026-10-17T10:01:00Z",
            },
            {"body": "unattributed event"},
        ]
        result = TemplateAggregator(max_templates=10).aggregate(rows)
        self.assertEqual(result.rows_seen, 3)
        self.assertTrue(result.unknown)
        self.assertIn("service.name", result.unknown_fields)
        self.assertEqual(result.templates[0].service, "checkout")
        self.assertEqual(result.templates[0].count, 2)
        self.assertNotIn("secret-value", result.tsv())
        self.assertIn("<REDACTED>", result.tsv())

    def test_reports_template_truncation(self) -> None:
        rows = [
            {"service.name": "svc", "severity_text": "INFO", "body": body}
            for body in ("event alpha", "event beta", "event gamma")
        ]
        result = TemplateAggregator(max_templates=2).aggregate(rows, source_truncated=True)
        self.assertTrue(result.truncated)
        self.assertEqual(result.truncation_reasons, ["source_row_limit", "template_limit"])
        self.assertEqual(len(result.templates), 2)
