import json
import unittest

from run import REQUIRED_TESTS, evaluate_tests


class EvidenceTests(unittest.TestCase):
    def output(self, names):
        return "\n".join(json.dumps({"Action": "pass", "Test": name}) for name in names)

    def test_every_required_protocol_must_have_an_actual_pass(self):
        _, counts = evaluate_tests(self.output(REQUIRED_TESTS), 0)
        self.assertEqual(13, counts["pass"])
        with self.assertRaises(RuntimeError):
            evaluate_tests(self.output(REQUIRED_TESTS - {"TestTURNRelayTransports/TLS"}), 0)

    def test_skipped_failed_or_zero_test_runs_never_pass(self):
        good = self.output(REQUIRED_TESTS)
        for action in ("skip", "fail"):
            with self.assertRaises(RuntimeError):
                evaluate_tests(good + "\n" + json.dumps({"Action": action, "Test": "negative-path"}), 0)
        with self.assertRaises(RuntimeError):
            evaluate_tests("", 0)

    def test_process_failure_cannot_be_hidden_by_successful_test_records(self):
        with self.assertRaises(RuntimeError):
            evaluate_tests(self.output(REQUIRED_TESTS), 1)


if __name__ == "__main__":
    unittest.main()
