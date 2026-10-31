import importlib.util
import pathlib
import unittest

spec = importlib.util.spec_from_file_location('coverage_check', pathlib.Path(__file__).with_name('tvos-changed-coverage.py'))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class CoverageTests(unittest.TestCase):
    def test_added_lines_and_missing_source(self):
        edits = module.changed('+++ b/a.go\n@@ -1,0 +2,2 @@\n+new\n+lines')
        self.assertEqual(edits, {'a.go': {2, 3}})
        self.assertEqual(module.report(edits, {}, 95)['missing'], ['a.go'])

    def test_partial_blocks_and_thresholds(self):
        counts = module.counters('mode: set\nexample/a.go:1.1,3.2 2 1\nexample/a.go:3.2,4.3 1 0\nexample/b.go:1.1,2.2 0 0', 'example')
        self.assertEqual(counts['a.go'], {1: 1, 2: 1, 3: 0, 4: 0})
        self.assertFalse(module.report({'a.go': {1, 2, 3, 4}}, counts, 95)['passed'])
        self.assertTrue(module.report({'a.go': {1, 2}}, counts, 95)['passed'])
        self.assertFalse(module.report({'a_test.go': {1}}, counts, 95)['passed'])

    def test_malformed_profile(self):
        with self.assertRaises(ValueError):
            module.counters('mode: set\nbroken', 'example')


if __name__ == '__main__':
    unittest.main()
